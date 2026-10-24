package migrationlint

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// secretValuePrefix starts the name of every environment variable a secret's
// value comes from. Ptah defines `$<variable>` in the query that creates or
// alters the secret, reading the variable where the migration runs, so the
// value is never written down.
const secretValuePrefix = "PTAH_SECRET_"

// clearSecretValue reports whether statement, one YQL statement, puts a
// secret's value into its own text: a CREATE SECRET or ALTER SECRET whose
// value is anything but a reference Ptah defines, or any statement that
// creates or changes the deprecated `OBJECT ... (TYPE SECRET)`. It returns the
// statement's form and the secret's path as written, and never the value.
func clearSecretValue(statement string) (form, path string, writes bool) {
	tokens := secretTokens(statement)
	if form, path, legacy := legacySecretObject(tokens); legacy {
		return form, path, true
	}
	header, ok := secretHeader(tokens)
	if !ok {
		return "", "", false
	}
	form = strings.ToUpper(tokens[0].Value) + " SECRET"
	path = identifierText(tokens[header])
	for _, position := range secretValueOptions(tokens) {
		variable, isReference := secretReference(tokens[position])
		if !isReference || !validValueVariable(variable) {
			return form, path, true
		}
	}
	return form, path, false
}

// secretTokens reads a statement as YQL, leaving out whitespace and comments.
func secretTokens(statement string) []yql.Token {
	var tokens []yql.Token
	for _, token := range yql.Tokens(statement) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment:
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}

// secretReference returns the variable a token refers to as a secret value: a
// named expression whose name starts with [secretValuePrefix] in any case.
func secretReference(token yql.Token) (string, bool) {
	if token.Type != yql.TokenIdentifier || len(token.Value) <= len(secretValuePrefix) ||
		!strings.EqualFold(token.Value[:len(secretValuePrefix)+1], "$"+secretValuePrefix) {
		return "", false
	}
	return token.Value[1:], true
}

// validValueVariable reports a variable Ptah reads a secret's value from: it
// starts with [secretValuePrefix] exactly, has a name after it, and holds only
// ASCII letters, digits and underscores.
func validValueVariable(name string) bool {
	rest, prefixed := strings.CutPrefix(name, secretValuePrefix)
	if !prefixed || rest == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// secretHeader reads `CREATE SECRET`, `CREATE OR REPLACE SECRET` (with an
// optional IF NOT EXISTS) or `ALTER SECRET` (with an optional IF EXISTS) at
// the start of statement, and returns the position of the path after it.
func secretHeader(statement []yql.Token) (int, bool) {
	words := func(i int, want ...string) bool {
		if i+len(want) > len(statement) {
			return false
		}
		for k, word := range want {
			if !statement[i+k].IsWord(word) {
				return false
			}
		}
		return true
	}
	var i int
	switch {
	case words(0, "CREATE", "OR", "REPLACE", "SECRET"):
		i = 4
	case words(0, "CREATE", "SECRET"), words(0, "ALTER", "SECRET"):
		i = 2
	default:
		return 0, false
	}
	switch {
	case statement[0].IsWord("CREATE") && words(i, "IF", "NOT", "EXISTS"):
		i += 3
	case statement[0].IsWord("ALTER") && words(i, "IF", "EXISTS"):
		i += 2
	}
	return i, i < len(statement)
}

// secretValueOptions returns the positions of every `value` option's value
// directly inside the WITH list of a CREATE SECRET or ALTER SECRET.
func secretValueOptions(statement []yql.Token) []int {
	i, ok := secretHeader(statement)
	if !ok {
		return nil
	}
	i++
	if i+1 >= len(statement) || !statement[i].IsWord("WITH") || !statement[i+1].IsOperator("(") {
		return nil
	}
	var positions []int
	depth := 0
	for j := i + 1; j < len(statement); j++ {
		switch {
		case statement[j].IsOperator("("):
			depth++
			continue
		case statement[j].IsOperator(")"):
			depth--
			continue
		}
		if depth == 1 && j >= i+4 && statement[j-1].IsOperator("=") && statement[j-2].IsWord("value") {
			positions = append(positions, j)
		}
	}
	return positions
}

// legacySecretObject reads `CREATE|ALTER|UPSERT OBJECT <name> (TYPE SECRET)`
// at the start of statement and returns its form and the name as written.
func legacySecretObject(statement []yql.Token) (form, name string, ok bool) {
	if len(statement) < 7 || !statement[1].IsWord("OBJECT") {
		return "", "", false
	}
	verb := strings.ToUpper(statement[0].Value)
	if verb != "CREATE" && verb != "ALTER" && verb != "UPSERT" {
		return "", "", false
	}
	i := 2
	if verb == "CREATE" && len(statement) > 5 && statement[2].IsWord("IF") {
		i = 5
	}
	if i+4 >= len(statement) || !statement[i+1].IsOperator("(") || !statement[i+2].IsWord("TYPE") ||
		!statement[i+3].IsWord("SECRET") || !statement[i+4].IsOperator(")") {
		return "", "", false
	}
	return verb + " OBJECT ... (TYPE SECRET)", identifierText(statement[i]), true
}

// identifierText is a name token as a reader writes it: a backticked path
// without its quotes.
func identifierText(token yql.Token) string {
	if name, ok := yql.IdentifierValue(token.Value); ok {
		return name
	}
	return token.Value
}

// deprecatedSecretNames reads statement, one YQL statement, and when it
// creates an external data source, returns the source's path as written and
// every option of it that names a credential by a deprecated secret object:
// an option whose name ends in _SECRET_NAME.
func deprecatedSecretNames(statement string) (string, []string) {
	tokens := secretTokens(statement)
	i := 0
	next := func(words ...string) bool {
		for _, word := range words {
			if i >= len(tokens) || !tokens[i].IsWord(word) {
				return false
			}
			i++
		}
		return true
	}
	if !next("CREATE") {
		return "", nil
	}
	next("OR", "REPLACE")
	if !next("EXTERNAL", "DATA", "SOURCE") {
		return "", nil
	}
	next("IF", "NOT", "EXISTS")
	if i >= len(tokens) {
		return "", nil
	}
	source := identifierText(tokens[i])
	var options []string
	depth := 0
	for j := i + 1; j < len(tokens); j++ {
		token := tokens[j]
		switch {
		case token.IsOperator("("):
			depth++
		case token.IsOperator(")"):
			depth--
		case depth == 1 && token.Type == yql.TokenIdentifier && j+1 < len(tokens) &&
			tokens[j+1].IsOperator("=") && strings.HasSuffix(strings.ToUpper(token.Value), "_SECRET_NAME"):
			options = append(options, strings.ToUpper(token.Value))
		}
	}
	return source, options
}
