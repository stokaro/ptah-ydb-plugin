// Package servicequery finds the statements this provider runs through a YDB
// service rather than the query service.
//
// YQL has no statement for a comment or a coordination node. Ptah's YDB
// connection reads `COMMENT ON ...` and `CREATE`, `ALTER` and `DROP
// COORDINATION NODE` itself and runs each through the table or the
// coordination service. A query holding one of them holds nothing else but
// definitions ahead of it, the way the host's connection reads it, so that a
// migration file written for the host's embedded engine runs the same way
// through this provider. [Locate] is that rule; the packages that read each
// statement decide what opens one.
package servicequery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// Located is a service statement as a query holds it.
type Located struct {
	// Tokens are the statement's significant tokens.
	Tokens []yql.Token
	// PathPrefix is the path the query's `PRAGMA TablePathPrefix` sets ahead
	// of the statement, or empty.
	PathPrefix string
}

// Locate reports whether text is a query running a statement opens
// recognizes, preceded by nothing but definitions, and returns it. A query
// holding two such statements, one that is not last, or one after a
// statement that is not a definition is refused, naming service as the YDB
// service that takes the statement alone.
func Locate(text string, opens func([]yql.Token) bool, service string) (Located, bool, error) {
	statements := splitSourceStatements(text)
	tokens := make([][]yql.Token, len(statements))
	found := -1
	for i, statement := range statements {
		tokens[i] = significant(statement)
		if len(tokens[i]) == 0 || !opens(tokens[i]) {
			continue
		}
		if found >= 0 {
			return Located{}, true, fmt.Errorf("a query runs one %s statement, and this one holds two; "+
				"put each in a query of its own", FirstWords(tokens[i], 3))
		}
		found = i
	}
	if found < 0 {
		return Located{}, false, nil
	}
	opening := FirstWords(tokens[found], 3)
	if found != len(statements)-1 {
		return Located{}, true, fmt.Errorf("%s is followed by another statement in its query, and Ptah runs "+
			"it through YDB's %s, which takes it alone", opening, service)
	}
	result := Located{Tokens: tokens[found]}
	for _, definition := range tokens[:found] {
		prefix, isPrefix, err := readDefinition(definition, opening, service)
		if err != nil {
			return Located{}, true, err
		}
		if isPrefix {
			result.PathPrefix = prefix
		}
	}
	return result, true, nil
}

// ContainsFold reports whether text contains word, ignoring case, without
// copying text: every query a connection runs is asked.
func ContainsFold(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if strings.EqualFold(text[i:i+len(word)], word) {
			return true
		}
	}
	return false
}

// significant are the tokens of a statement that carry meaning: not
// whitespace, not comments, not a terminator, and not the translation setting
// at the head of a query.
func significant(statement string) []yql.Token {
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

// FirstWords names a statement by up to count of its opening words, upper
// case, for a message.
func FirstWords(tokens []yql.Token, count int) string {
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
			"which takes it alone", statement, FirstWords(tokens, 3), service)
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
	text, ok := StringValue(value)
	if !ok {
		return "", false, fmt.Errorf("%s follows a TablePathPrefix pragma whose path is not a plain "+
			"string", statement)
	}
	return text, true, nil
}

// StringValue reads a single- or double-quoted string token, with or without
// the Utf8 suffix, into the text it denotes. It undoes a backslash before a
// backslash or either quote, `\n`, `\r`, `\t`, and `\x` with two hex digits,
// and reports false for any other escape.
func StringValue(token yql.Token) (string, bool) {
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
