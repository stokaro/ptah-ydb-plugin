package yql

import (
	"slices"
	"strings"
)

// AlteredTable returns the table an ALTER TABLE text changes, as the text
// writes it: a path relative to the root the session resolves names against,
// or an absolute one. It answers only where the text alone settles the
// answer, as the host's internal/yqlquery.AlteredTable does, and reports false
// otherwise: when the text is not one ALTER TABLE statement, when the table is
// a named expression (`ALTER TABLE $t ...`), when a quoted name holds an
// escape the server refuses, and when the text holds a PRAGMA, since `PRAGMA
// TablePathPrefix` changes what a relative name resolves to. A named
// expression defined ahead of the statement does not count as a statement.
func AlteredTable(text string) (string, bool) {
	var statements [][]Token
	starts := true
	for _, token := range Tokens(text) {
		switch token.Type {
		case TokenWhitespace, TokenComment:
			continue
		case TokenUnknown:
			// A translation setting heads the text and starts no statement.
			continue
		case TokenSemicolon:
			starts = true
			continue
		}
		if starts {
			statements = append(statements, nil)
			starts = false
		}
		statements[len(statements)-1] = append(statements[len(statements)-1], token)
	}
	// A named expression defined ahead of the statement is carried into its
	// query, as the host's query split carries it, and changes no path.
	statements = slices.DeleteFunc(statements, namedExpression)
	if len(statements) != 1 {
		return "", false
	}
	significant := statements[0]
	if len(significant) < 3 || !significant[0].IsWord("ALTER") || !significant[1].IsWord("TABLE") {
		return "", false
	}
	table := significant[2]
	if table.Type != TokenIdentifier || strings.HasPrefix(table.Value, "$") {
		return "", false
	}
	return IdentifierValue(table.Value)
}

// namedExpression reports a statement that defines a named expression,
// `$name = ...`.
func namedExpression(tokens []Token) bool {
	return len(tokens) > 1 && tokens[0].Type == TokenIdentifier && strings.HasPrefix(tokens[0].Value, "$") && tokens[1].IsOperator("=")
}
