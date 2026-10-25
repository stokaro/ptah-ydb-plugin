// Package yqlquery splits a YQL text into the queries YDB runs it as, as the
// Ptah host's internal/yqlquery does for its embedded YDB engine.
//
// YDB cannot run an arbitrary script as one query. A scheme statement --
// CREATE, ALTER, DROP, GRANT, REVOKE, TRUNCATE and the rest -- is refused inside
// a transaction and cannot share a query with a data statement. A named
// expression (`$x = ...`), an action (DEFINE ACTION), a DECLARE and a PRAGMA,
// on the other hand, exist only in the query that holds them. Measured by the
// host on YDB 26.2.1.14:
//
//	$x = 9l;                                  one query: accepted
//	UPSERT INTO t (id) VALUES ($x);           the next query: Unknown name: $x
//	UPSERT ...; TRUNCATE TABLE t;             Queries with mixed data and scheme operations are not supported
//
// So [Split] makes each scheme statement a query of its own, and so is a block
// or an action call that runs one. Each BATCH UPDATE and BATCH DELETE is a
// query of its own too, since YDB runs one only outside a transaction and
// alone in its query. Each run of consecutive data statements is one query.
// Every definition -- PRAGMA, DECLARE, a named expression, DEFINE ACTION and
// DEFINE SUBQUERY -- is carried into every query that follows it in the text,
// which then sees what the text declared above it. A translation setting at
// the head of the text (`--!syntax_v1`) is honored by YDB only at the head of
// a query, so it heads every query.
package yqlquery

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// Kind says how YDB runs a query.
type Kind uint8

const (
	// Data is a query of data statements: it runs as one transaction, which
	// commits whole or not at all.
	Data Kind = iota + 1
	// Scheme is a query of one scheme statement, which YDB runs outside any
	// transaction. A block or an action call that runs a scheme statement is
	// one too.
	Scheme
	// Batch is a query of one BATCH UPDATE or BATCH DELETE, which YDB runs only
	// in the implicit transaction mode, as the only write or read of its query.
	Batch
)

// String names the kind for a message.
func (k Kind) String() string {
	switch k {
	case Data:
		return "data"
	case Scheme:
		return "scheme"
	case Batch:
		return "batch"
	default:
		return "unknown"
	}
}

// Query is one query a YQL text runs as.
type Query struct {
	// Kind says whether the query is a scheme statement or data statements.
	Kind Kind
	// Text is what is sent to YDB: the translation settings at the head of
	// the text, the definitions the text made before this query, and this
	// query's own statements, separated by semicolons.
	Text string
	// Statements are the query's own statements as they run, without the
	// definitions carried in from above it, comments removed and with no
	// terminator.
	Statements []string
	// Start is the byte offset in the text of the first statement of the
	// query that is its own or a definition it opens with.
	Start int
	// Mixed reports that YDB refuses the query whole: it is a block or an
	// action call that runs a scheme statement and a statement that reads or
	// writes a table. Only a [Scheme] query can be one.
	Mixed bool
}

// ErrTranslationSetting is returned for a translation setting other than
// --!syntax_v1 at the head of a text.
var ErrTranslationSetting = errors.New("unsupported YQL translation setting")

// ErrClientDelimiter is returned for a client delimiter directive in a text.
var ErrClientDelimiter = errors.New("client delimiter directive in YQL")

// supportedSetting is the one translation setting a text is read under.
// --!syntax_pg and --!syntax_v0 are disabled on the server, and --!ansi_lexer
// changes how every string, name and comment after it is read, which the
// scanner does not model.
const supportedSetting = "--!syntax_v1"

// Refusal is a text Split cannot split the way YDB reads it, with where the
// part it refuses starts.
type Refusal struct {
	// Offset is the byte offset of the refused setting or directive.
	Offset int
	err    error
}

func (r *Refusal) Error() string { return r.err.Error() }

// Unwrap identifies the refusal: [ErrTranslationSetting] or
// [ErrClientDelimiter].
func (r *Refusal) Unwrap() error { return r.err }

// Split returns the queries text runs as, in order. A text with no statement
// returns no query.
//
// It refuses, with a *[Refusal], what it cannot split the way YDB reads the
// text: a translation setting other than --!syntax_v1, and a client delimiter
// directive. YQL has no client delimiter, so `DELIMITER $$` and `-- atlas:delimiter
// $$` would be ignored, and the author's intent with them.
func Split(text string) ([]Query, error) {
	tokens := yql.Tokens(text)
	settings, err := translationSettings(tokens)
	if err != nil {
		return nil, err
	}
	if err := refuseClientDelimiters(tokens); err != nil {
		return nil, err
	}
	header := ""
	if len(settings) > 0 {
		header = strings.Join(settings, "\n") + "\n"
	}
	builder := splitter{header: header}
	for _, source := range statements(text, tokens) {
		statement := executable(source.tokens)
		if statement == "" {
			continue
		}
		builder.add(statement, source.start)
	}
	return builder.finish(), nil
}

// source is the tokens of one statement of a text, through its semicolon, and
// where its first significant token starts.
type source struct {
	tokens []yql.Token
	start  int
}

// statements cuts a text at the semicolons that end a statement. A semicolon
// inside a string, a comment or a body that holds statements -- an action, an
// inline block or a lambda -- ends none.
func statements(text string, tokens []yql.Token) []source {
	var out []source
	var current []yql.Token
	start := -1
	var bodies yql.Bodies
	for _, token := range tokens {
		current = append(current, token)
		switch token.Type {
		case yql.TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case yql.TokenOperator:
			bodies.Symbol(token.Value)
		}
		if start < 0 && significant(token) {
			start = token.Start
		}
		if token.Type != yql.TokenSemicolon {
			continue
		}
		bodies.Separate()
		if bodies.Inside() {
			continue
		}
		bodies = yql.Bodies{}
		out = append(out, source{tokens: current, start: max(start, 0)})
		current, start = nil, -1
	}
	if len(current) > 0 {
		out = append(out, source{tokens: current, start: max(start, len(text))})
	}
	return out
}

// significant reports a token that is not whitespace, a comment or a
// translation setting.
func significant(token yql.Token) bool {
	switch token.Type {
	case yql.TokenWhitespace, yql.TokenComment, yql.TokenUnknown:
		return false
	default:
		return true
	}
}

// executable is a statement's text as it runs: comments and translation
// settings removed and its terminator dropped.
func executable(tokens []yql.Token) string {
	var b strings.Builder
	for _, token := range tokens {
		switch token.Type {
		case yql.TokenComment, yql.TokenUnknown:
			continue
		}
		b.WriteString(token.Value)
	}
	stripped := strings.TrimSpace(b.String())
	stripped = strings.TrimSuffix(stripped, ";")
	return strings.TrimSpace(stripped)
}

// splitter accumulates the queries of one text.
type splitter struct {
	header string
	// carried are the definitions made so far, in order.
	carried []string
	// pendingStart is where the first definition made since the last query
	// closed starts, or -1.
	pendingStart int
	pendingCount int
	// open is the run of data statements being collected, if any.
	open   *draft
	drafts []draft
	// reader classifies each statement, remembering the actions the text
	// defined so far.
	reader Reader
}

// draft is a query being assembled.
type draft struct {
	kind       Kind
	mixed      bool
	prefix     []string
	statements []string
	start      int
}

func (s *splitter) add(statement string, start int) {
	if s.pendingCount == 0 {
		s.pendingStart = -1
	}
	read := s.reader.read(statement)
	switch kind := read.kind; kind {
	case definition:
		s.carried = append(s.carried, statement)
		if s.open != nil {
			s.open.statements = append(s.open.statements, statement)
			return
		}
		if s.pendingCount == 0 {
			s.pendingStart = start
		}
		s.pendingCount++
	case Scheme, Batch:
		s.close()
		query := s.start(kind, start)
		query.mixed = read.mixed
		query.statements = append(query.statements, statement)
		s.drafts = append(s.drafts, query)
	case Data:
		if s.open == nil {
			query := s.start(Data, start)
			s.open = &query
		}
		s.open.statements = append(s.open.statements, statement)
	}
}

// start opens a query of kind, headed by the definitions made before it. It
// starts at its first pending definition, or at its own first statement.
func (s *splitter) start(kind Kind, start int) draft {
	if s.pendingCount > 0 {
		start = s.pendingStart
	}
	query := draft{kind: kind, prefix: slices.Clip(s.carried), start: start}
	s.pendingCount = 0
	return query
}

// close ends the run of data statements being collected.
func (s *splitter) close() {
	if s.open == nil {
		return
	}
	s.drafts = append(s.drafts, *s.open)
	s.open = nil
}

// finish closes the last run and places definitions that no statement
// followed. They join the last query, after its statements, where they run
// and change nothing; a text of nothing but definitions is one data query.
func (s *splitter) finish() []Query {
	s.close()
	if s.pendingCount > 0 {
		trailing := s.carried[len(s.carried)-s.pendingCount:]
		if len(s.drafts) == 0 {
			s.drafts = append(s.drafts, draft{kind: Data, start: s.pendingStart})
		}
		last := &s.drafts[len(s.drafts)-1]
		last.statements = append(last.statements, trailing...)
	}
	queries := make([]Query, 0, len(s.drafts))
	for _, query := range s.drafts {
		parts := append(slices.Clone(query.prefix), query.statements...)
		queries = append(queries, Query{
			Kind:       query.kind,
			Text:       s.header + strings.Join(parts, ";\n"),
			Statements: query.statements,
			Start:      query.start,
			Mixed:      query.mixed,
		})
	}
	return queries
}

// translationSettings returns the --! settings at the head of text, and
// refuses one a text is not read under.
func translationSettings(tokens []yql.Token) ([]string, error) {
	var settings []string
	for _, token := range tokens {
		switch {
		case token.Type == yql.TokenWhitespace:
			continue
		case token.Type == yql.TokenUnknown && strings.HasPrefix(token.Value, "--!"):
			setting := strings.TrimSpace(token.Value)
			if setting != supportedSetting {
				return nil, &Refusal{Offset: token.Start, err: fmt.Errorf("%w: YQL is split as --!syntax_v1 reads it, "+
					"and YDB would read this text differently or refuse it; remove the line", ErrTranslationSetting)}
			}
			settings = append(settings, setting)
		default:
			return settings, nil
		}
	}
	return settings, nil
}

// refuseClientDelimiters refuses a delimiter directive written as a comment or
// as a statement of its own. A directive inside a string literal is text, and
// the scanner reads it as part of the literal.
func refuseClientDelimiters(tokens []yql.Token) error {
	statementStart := true
	for _, token := range tokens {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenUnknown:
			continue
		case yql.TokenComment:
			if atlasDelimiter(token.Value) {
				return delimiterRefusal(token.Start)
			}
			continue
		case yql.TokenSemicolon:
			statementStart = true
			continue
		}
		if statementStart && token.IsWord("DELIMITER") {
			return delimiterRefusal(token.Start)
		}
		statementStart = false
	}
	return nil
}

// atlasDelimiter reports an `-- atlas:delimiter` directive comment: the
// keyword in any letter case, whitespace, and a delimiter.
func atlasDelimiter(comment string) bool {
	const keyword = "-- atlas:delimiter"
	trimmed := strings.TrimSpace(comment)
	if len(trimmed) <= len(keyword) || !strings.EqualFold(trimmed[:len(keyword)], keyword) {
		return false
	}
	return strings.ContainsRune(" \t\n\r", rune(trimmed[len(keyword)])) && strings.TrimSpace(trimmed[len(keyword):]) != ""
}

func delimiterRefusal(offset int) error {
	return &Refusal{Offset: offset, err: fmt.Errorf("%w: a delimiter directive has no meaning in YQL, which ends every statement "+
		"with a semicolon; remove the directive and terminate each statement with a semicolon", ErrClientDelimiter)}
}
