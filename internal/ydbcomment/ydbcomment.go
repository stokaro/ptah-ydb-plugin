// Package ydbcomment reads the statements Ptah keeps a YDB comment with.
//
// YQL has no COMMENT statement. Ptah keeps a comment as a user attribute of
// the table or view it belongs to, and a column's and an index's comment on
// their table under a key that names them, and its YDB connection runs
// `COMMENT ON TABLE`, `COLUMN`, `INDEX ... ON` and `VIEW` through the table
// service. [Recognize] reads those statements as the host's
// internal/ydbcomment does, for migration lint and for the YQL schema reader,
// so both read a comment with one rule.
package ydbcomment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// Object is what a comment belongs to.
type Object int

const (
	Table Object = iota + 1
	Column
	Index
	View
)

// keyword is the word COMMENT ON names the object with.
func (o Object) Keyword() string {
	switch o {
	case Table:
		return "TABLE"
	case Column:
		return "COLUMN"
	case Index:
		return "INDEX"
	case View:
		return "VIEW"
	default:
		return ""
	}
}

// noun names the object in a sentence, with its article.
func (o Object) noun() string {
	if o == Index {
		return "an index"
	}
	return "a " + strings.ToLower(o.Keyword())
}

// The attribute keys: a table's and a view's own comment, and the prefixes a
// column's and an index's name follow.
const (
	ownCommentKey    = "ptah.comment"
	columnCommentKey = "ptah.comment.column."
	indexCommentKey  = "ptah.comment.index."
)

// YDB's limits on a user attribute, measured on 25.1.4.7 and 26.2.1.14.
const (
	maxCommentKeyBytes   = 100
	maxCommentValueBytes = 4096
)

// Key is the attribute that holds the comment of object, where name is
// the column's or the index's name.
func Key(object Object, name string) string {
	switch object {
	case Column:
		return columnCommentKey + name
	case Index:
		return indexCommentKey + name
	default:
		return ownCommentKey
	}
}

// Refusal says why YDB cannot hold comment on object, or returns ""
// when it can.
func Refusal(object Object, name, comment string) string {
	key := Key(object, name)
	switch {
	case (object == Column || object == Index) && name == "":
		return fmt.Sprintf("%s comment names no %s", object.noun(), strings.ToLower(object.Keyword()))
	case len(key) > maxCommentKeyBytes:
		return fmt.Sprintf("YDB keeps the comment as the table attribute %q, %d bytes long, and an attribute key "+
			"takes at most %d bytes; %s name takes at most %d", key, len(key), maxCommentKeyBytes,
			object.noun(), maxCommentKeyBytes-len(Key(object, "")))
	case len(comment) > maxCommentValueBytes:
		return fmt.Sprintf("the comment is %d bytes long, and YDB keeps at most %d bytes in an attribute value",
			len(comment), maxCommentValueBytes)
	case !utf8.ValidString(comment):
		return "the comment is not UTF-8 text, which YDB keeps in an attribute value"
	}
	return ""
}

// Statement is one of Ptah's comment statements.
type Statement struct {
	Object Object
	// Path is the table's or the view's path as the statement names it.
	Path string
	// Name is the column's or the index's name.
	Name string
	// Comment is what the comment becomes; empty removes it.
	Comment string
}

// Key is the attribute the statement sets.
func (s Statement) Key() string {
	return Key(s.Object, s.Name)
}

// Query is a query that runs one of Ptah's comment statements.
type Query struct {
	Statement
	// PathPrefix is the path the query's `PRAGMA TablePathPrefix` sets, or
	// empty where it sets none.
	PathPrefix string
}

var ErrStatement = errors.New("invalid comment statement")

// service is the YDB service a comment statement runs through, as a
// refusal names it.
const service = "table service"

// Recognize reports whether text is a query running one of Ptah's
// comment statements, and reads it. Recognized text that does not read as the
// statement is refused.
func Recognize(text string) (Query, bool, error) {
	if !containsFold(text, "comment") {
		return Query{}, false, nil
	}
	located, recognized, err := locateServiceStatement(text, opens, service)
	switch {
	case err != nil:
		return Query{}, true, fmt.Errorf("%w: %w", ErrStatement, err)
	case !recognized:
		return Query{}, false, nil
	}
	statement, err := parse(located.tokens)
	if err != nil {
		return Query{}, true, err
	}
	return Query{Statement: statement, PathPrefix: located.pathPrefix}, true, nil
}

// containsFold reports whether text contains word, ignoring case.
func containsFold(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if strings.EqualFold(text[i:i+len(word)], word) {
			return true
		}
	}
	return false
}

// opens reports whether tokens open one of the four statements.
func opens(tokens []yql.Token) bool {
	if len(tokens) < 3 || !tokens[0].IsWord("COMMENT") || !tokens[1].IsWord("ON") {
		return false
	}
	_, known := objectOf(tokens[2])
	return known
}

// objectOf reads the object keyword of a statement.
func objectOf(token yql.Token) (Object, bool) {
	for _, object := range []Object{Table, Column, Index, View} {
		if token.IsWord(object.Keyword()) {
			return object, true
		}
	}
	return 0, false
}

// parse reads one of the four statements from its tokens.
func parse(tokens []yql.Token) (Statement, error) {
	object, _ := objectOf(tokens[2])
	statement := Statement{Object: object}
	opening := firstWords(tokens, 3)
	refuse := func(format string, args ...any) (Statement, error) {
		return Statement{}, fmt.Errorf("%w: %s: %s", ErrStatement, opening, fmt.Sprintf(format, args...))
	}
	rest := tokens[3:]
	var ok bool
	switch object {
	case Column:
		if len(rest) < 3 || !rest[1].IsOperator(".") {
			return refuse("name the column as `table`.`column`")
		}
		if statement.Path, ok = name(rest[0]); !ok {
			return refuse("%s does not name a table; write its path in backticks", rest[0].Value)
		}
		if statement.Name, ok = name(rest[2]); !ok {
			return refuse("%s does not name a column", rest[2].Value)
		}
		rest = rest[3:]
	case Index:
		if len(rest) < 3 || !rest[1].IsWord("ON") {
			return refuse("name the index as `index` ON `table`")
		}
		if statement.Name, ok = name(rest[0]); !ok {
			return refuse("%s does not name an index", rest[0].Value)
		}
		if statement.Path, ok = name(rest[2]); !ok {
			return refuse("%s does not name a table; write its path in backticks", rest[2].Value)
		}
		rest = rest[3:]
	default:
		if len(rest) == 0 {
			return refuse("the statement names no %s", strings.ToLower(object.Keyword()))
		}
		if statement.Path, ok = name(rest[0]); !ok {
			return refuse("%s does not name %s; write its path in backticks", rest[0].Value, object.noun())
		}
		rest = rest[1:]
	}
	if len(rest) != 2 || !rest[0].IsWord("IS") {
		return refuse("the comment follows IS, as a string or NULL, and nothing follows it")
	}
	if !rest[1].IsWord("NULL") {
		comment, isString := serviceStringValue(rest[1])
		if !isString {
			return refuse("the comment %s is not a string Ptah reads; write it in quotes, with the escapes "+
				"\\\\, \\', \\\", \\n, \\r, \\t and \\xHH", rest[1].Value)
		}
		statement.Comment = comment
	}
	if reason := Refusal(statement.Object, statement.Name, statement.Comment); reason != "" {
		return refuse("%s", reason)
	}
	return statement, nil
}

// name reads a backticked or plain identifier. A named expression is
// not a name: the statement is read before YDB would evaluate one.
func name(token yql.Token) (string, bool) {
	if token.Type != yql.TokenIdentifier || strings.HasPrefix(token.Value, "$") {
		return "", false
	}
	value, ok := yql.IdentifierValue(token.Value)
	return value, ok && value != ""
}

// located is one of Ptah's own statements as a query holds it.
type located struct {
	// tokens are the statement's significant tokens.
	tokens []yql.Token
	// pathPrefix is the path the query's `PRAGMA TablePathPrefix` sets ahead
	// of the statement, or empty.
	pathPrefix string
}

// locateServiceStatement reports whether text is a query running a statement
// opens recognizes, preceded by nothing but definitions, and returns it. A
// query holding two such statements, one that is not last, or one after a
// statement that is not a definition is refused.
func locateServiceStatement(text string, opens func([]yql.Token) bool, service string) (located, bool, error) {
	statements := splitSourceStatements(text)
	tokens := make([][]yql.Token, len(statements))
	found := -1
	for i, statement := range statements {
		tokens[i] = serviceTokens(statement)
		if len(tokens[i]) == 0 || !opens(tokens[i]) {
			continue
		}
		if found >= 0 {
			return located{}, true, fmt.Errorf("a query runs one %s statement, and this one holds two; "+
				"put each in a query of its own", firstWords(tokens[i], 3))
		}
		found = i
	}
	if found < 0 {
		return located{}, false, nil
	}
	opening := firstWords(tokens[found], 3)
	if found != len(statements)-1 {
		return located{}, true, fmt.Errorf("%s is followed by another statement in its query, and Ptah runs "+
			"it through YDB's %s, which takes it alone", opening, service)
	}
	result := located{tokens: tokens[found]}
	for _, definition := range tokens[:found] {
		prefix, isPrefix, err := readDefinition(definition, opening, service)
		if err != nil {
			return located{}, true, err
		}
		if isPrefix {
			result.pathPrefix = prefix
		}
	}
	return result, true, nil
}

// serviceTokens are the tokens of a statement that carry meaning: not
// whitespace, not comments, not a terminator, and not the translation setting
// at the head of a query.
func serviceTokens(statement string) []yql.Token {
	var tokens []yql.Token
	for _, token := range yql.Tokens(statement) {
		switch {
		case token.Type == yql.TokenWhitespace, token.Type == yql.TokenComment, token.Type == yql.TokenSemicolon:
			continue
		case token.Type == yql.TokenUnknown && strings.HasPrefix(token.Value, "--!"):
			continue
		default:
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// firstWords names a statement by up to count of its opening words, upper
// case, for a message.
func firstWords(tokens []yql.Token, count int) string {
	words := make([]string, 0, count)
	for _, token := range tokens[:min(count, len(tokens))] {
		words = append(words, strings.ToUpper(token.Value))
	}
	return strings.Join(words, " ")
}

// readDefinition checks that tokens are a definition, and reads the path a
// TablePathPrefix pragma sets.
func readDefinition(tokens []yql.Token, statement, service string) (prefix string, isPrefix bool, err error) {
	first := tokens[0]
	isNamed := first.Type == yql.TokenIdentifier && strings.HasPrefix(first.Value, "$")
	if !isNamed && !yqlquery.Defines(first) {
		return "", false, fmt.Errorf("%s shares its query with %s, and Ptah runs it through YDB's %s, "+
			"which takes it alone", statement, firstWords(tokens, 3), service)
	}
	if !first.IsWord("PRAGMA") || len(tokens) < 2 || !tokens[1].IsWord("TablePathPrefix") {
		return "", false, nil
	}
	rest := tokens[2:]
	var value yql.Token
	switch {
	case len(rest) == 3 && rest[0].IsOperator("(") && rest[2].IsOperator(")"):
		value = rest[1]
	case len(rest) == 2 && rest[0].IsOperator("="):
		value = rest[1]
	default:
		return "", false, fmt.Errorf("%s follows a TablePathPrefix pragma Ptah cannot read; "+
			"write it as PRAGMA TablePathPrefix(\"/database/path\")", statement)
	}
	text, ok := serviceStringValue(value)
	if !ok {
		return "", false, fmt.Errorf("%s follows a TablePathPrefix pragma whose path is not a plain "+
			"string", statement)
	}
	return text, true, nil
}

// serviceStringValue reads a single- or double-quoted string token, with or
// without the Utf8 suffix, into the text it denotes. It undoes a backslash
// before a backslash or either quote, `\n`, `\r`, `\t`, and `\x` with two hex
// digits, and reports false for any other escape.
func serviceStringValue(token yql.Token) (string, bool) {
	if token.Type != yql.TokenString {
		return "", false
	}
	text := strings.TrimSuffix(token.Value, "u")
	if len(text) < 2 || (text[0] != '\'' && text[0] != '"') || text[len(text)-1] != text[0] {
		return "", false
	}
	quote := text[0]
	body := text[1 : len(text)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == quote:
			return "", false
		case c != '\\':
			b.WriteByte(c)
			continue
		case i+1 == len(body):
			return "", false
		}
		i++
		switch body[i] {
		case '\\', '\'', '"':
			b.WriteByte(body[i])
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'x':
			if i+2 >= len(body) {
				return "", false
			}
			value, err := strconv.ParseUint(body[i+1:i+3], 16, 8)
			if err != nil {
				return "", false
			}
			b.WriteByte(byte(value))
			i += 2
		default:
			return "", false
		}
	}
	return b.String(), true
}

// splitSourceStatements returns the source text of each statement of sql as
// the migrator splits it: from the first meaningful token through the
// terminating semicolon, leading whitespace and comments left out, and a
// statement of nothing but trivia dropped.
func splitSourceStatements(sql string) []string {
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	var statements []string
	var current strings.Builder
	var bodies yql.Bodies
	for _, token := range yql.Tokens(sql) {
		if token.Type == yql.TokenSemicolon {
			bodies.Separate()
			if !bodies.Inside() {
				statements = appendSourceStatement(statements, current.String()+token.Value)
				current.Reset()
				bodies = yql.Bodies{}
				continue
			}
		}
		switch token.Type {
		case yql.TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case yql.TokenOperator:
			bodies.Symbol(token.Value)
		}
		current.WriteString(token.Value)
	}
	return appendSourceStatement(statements, current.String())
}

// appendSourceStatement trims the leading trivia off one statement and keeps
// it when anything is left.
func appendSourceStatement(statements []string, raw string) []string {
	text := trimLeadingTrivia(raw)
	if strings.TrimSpace(strings.TrimSuffix(text, ";")) == "" {
		return statements
	}
	return append(statements, text)
}

// trimLeadingTrivia drops leading whitespace and comments.
func trimLeadingTrivia(raw string) string {
	rest := raw
	for {
		trimmed := strings.TrimLeft(rest, " \t\r\n")
		switch {
		case strings.HasPrefix(trimmed, "--"):
			if end := strings.IndexAny(trimmed, "\r\n"); end >= 0 {
				rest = trimmed[end:]
				continue
			}
			return ""
		case strings.HasPrefix(trimmed, "/*"):
			if end := strings.Index(trimmed[2:], "*/"); end >= 0 {
				rest = trimmed[2+end+2:]
				continue
			}
			return ""
		default:
			rest = trimmed
		}
		break
	}
	if strings.TrimSpace(rest) == "" {
		return ""
	}
	return rest
}
