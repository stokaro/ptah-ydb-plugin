package migrationlint

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// tokenKind classifies the tokens the statement forms are built from.
type tokenKind int

const (
	tokWhitespace tokenKind = iota
	tokComment
	// tokString is a string literal, kept verbatim.
	tokString
	// tokQuotedIdent is a backticked name, kept verbatim with its quotes.
	tokQuotedIdent
	tokWord
	tokSemicolon
	tokOp
)

// lintToken is one token of a migration file.
type lintToken struct {
	kind tokenKind
	text string
	// start and end are byte offsets into the scanned input.
	start, end int
	// line is the 1-based line the token starts on.
	line int
}

// scan reads YQL with internal/yql, the lexer the migrator splits the same
// text with, and maps each token onto the kinds the rules read. A backticked
// name is a quoted identifier; every other identifier -- a keyword, a name, a
// $name, a number with its suffix -- is a word. A translation setting such as
// --!syntax_v1 at the head of the text is a comment here: it starts no
// statement. A UTF-8 byte order mark at the head is left out, so it does not
// become part of the first word.
func scan(input string) []lintToken {
	start := 0
	if strings.HasPrefix(input, "\uFEFF") {
		start = len("\uFEFF")
	}
	var tokens []lintToken
	line := 1
	for _, token := range yql.Tokens(input[start:]) {
		begin, end := start+token.Start, start+token.End
		tokens = append(tokens, lintToken{kind: kindOf(token), text: input[begin:end], start: begin, end: end, line: line})
		line += strings.Count(input[begin:end], "\n")
	}
	return tokens
}

// kindOf is the kind of one lexer token.
func kindOf(token yql.Token) tokenKind {
	switch token.Type {
	case yql.TokenWhitespace:
		return tokWhitespace
	case yql.TokenComment, yql.TokenUnknown:
		return tokComment
	case yql.TokenString:
		return tokString
	case yql.TokenSemicolon:
		return tokSemicolon
	case yql.TokenIdentifier:
		if strings.HasPrefix(token.Value, "`") {
			return tokQuotedIdent
		}
		return tokWord
	default:
		return tokOp
	}
}

// split cuts sql into its statements and renders each in the forms rules
// read.
//
// A semicolon inside a string or a comment does not end a statement, and
// neither does one inside a body that holds statements: an action, an inline
// block or a lambda ([yql.Bodies]). The words and the punctuation of the text
// are fed to the body tracker; a string literal is not.
//
// A statement's comments are those directly above it. A blank line detaches a
// comment from whatever comes next, and a comment on the line the previous
// statement ends on belongs to that statement, so a suppression directive has
// to sit directly above its statement.
func split(sql string) []Statement {
	var statements []Statement
	start, end, startLine, lastStatementEndLine := -1, 0, 0, 0
	var pending, active []string
	var bodies yql.Bodies
	emit := func() {
		statements = append(statements, render(sql[start:end], start, end, startLine, active))
	}
	for _, tok := range scan(sql) {
		switch tok.kind {
		case tokWord, tokQuotedIdent:
			bodies.Word(strings.ToUpper(tok.text))
		case tokOp:
			bodies.Symbol(tok.text)
		}
		switch tok.kind {
		case tokSemicolon:
			bodies.Separate()
			if bodies.Inside() {
				end = tok.end
				continue
			}
			bodies = yql.Bodies{}
			if start >= 0 {
				emit()
				start = -1
				active = nil
				lastStatementEndLine = tok.line
			}
		case tokWhitespace:
			if start < 0 && strings.Count(tok.text, "\n") > 1 {
				pending = nil
			}
		case tokComment:
			if start < 0 && tok.line != lastStatementEndLine {
				pending = append(pending, tok.text)
			}
		default:
			if start < 0 {
				start = tok.start
				startLine = tok.line
				active = slices.Clone(pending)
				pending = nil
			}
			end = tok.end
		}
	}
	if start >= 0 {
		emit()
	}
	return statements
}

// render is one statement's text in the forms rules read.
func render(text string, start, end, line int, comments []string) Statement {
	return Statement{
		Start: start, End: end, Line: line, SQL: text,
		Canonical: canonical(text), Words: words(text), SourceWords: sourceWords(text), Comments: comments,
	}
}

// canonical is a statement's display form: comments removed, every
// whitespace run collapsed to one space, everything except string literals and
// quoted names uppercased.
func canonical(sql string) string {
	var b strings.Builder
	pendingSpace := false
	for _, tok := range scan(sql) {
		switch tok.kind {
		case tokComment, tokWhitespace:
			// A comment separates tokens exactly like whitespace does.
			pendingSpace = b.Len() > 0
		case tokString, tokQuotedIdent:
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteString(tok.text)
		default:
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteString(strings.ToUpper(tok.text))
		}
	}
	return b.String()
}

// words is the word sequence rules scan: comments and whitespace dropped,
// bare words uppercased, punctuation as entries of its own, string literals
// and quoted names verbatim, so a quoted name never collides with a keyword.
func words(sql string) []string {
	var result []string
	for _, tok := range scan(sql) {
		switch tok.kind {
		case tokComment, tokWhitespace:
			continue
		case tokString, tokQuotedIdent:
			result = append(result, tok.text)
		default:
			result = append(result, strings.ToUpper(tok.text))
		}
	}
	return result
}

// sourceWords is [words] before the uppercasing, as the author spelled each
// token.
func sourceWords(sql string) []string {
	var result []string
	for _, tok := range scan(sql) {
		switch tok.kind {
		case tokComment, tokWhitespace:
			continue
		default:
			result = append(result, tok.text)
		}
	}
	return result
}
