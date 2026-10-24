package yql

import "strings"

// AlteredTable returns the table an ALTER TABLE text changes, as the text
// writes it: a path relative to the root the session resolves names against,
// or an absolute one. It answers only where the text alone settles the
// answer, as the host's internal/yqlquery.AlteredTable does, and reports false
// otherwise: when the text is not one ALTER TABLE statement, when the table is
// a named expression (`ALTER TABLE $t ...`), when a quoted name holds an
// escape the server refuses, and when the text holds a PRAGMA, since `PRAGMA
// TablePathPrefix` changes what a relative name resolves to.
func AlteredTable(text string) (string, bool) {
	var significant []Token
	statements := 0
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
			statements++
			starts = false
		}
		significant = append(significant, token)
	}
	if statements != 1 || len(significant) < 3 || !significant[0].IsWord("ALTER") || !significant[1].IsWord("TABLE") {
		return "", false
	}
	table := significant[2]
	if table.Type != TokenIdentifier || strings.HasPrefix(table.Value, "$") {
		return "", false
	}
	return IdentifierValue(table.Value)
}
