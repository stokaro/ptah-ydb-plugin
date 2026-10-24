package yql

import (
	"fmt"
	"slices"
	"strings"
)

// A dev realm is a directory a session treats as its whole database. The
// session sends `PRAGMA TablePathPrefix` with the realm's path before every
// query, so a relative path resolves inside the realm. What the pragma does not
// confine was measured on YDB 26.2.1.14 and 25.1.4.7, each statement sent after
// the pragma:
//
//	CREATE TABLE `../escape` (...)          created beside the realm
//	CREATE TABLE `/local/abs` (...)         created at the database root
//	ALTER TABLE t RENAME TO `../renamed`    moved the table out of the realm
//	$p = "/local/outside"; INSERT INTO $p   wrote a row outside the realm
//	PRAGMA TablePathPrefix = "/local"; ...  resolved names at the database root
//	CREATE USER u1 PASSWORD "p"             created a user of the database
//	GRANT SELECT ON t TO u1                 refused: a grant takes no prefix
//
// So a statement whose effect is not confined to the realm is refused: a write
// whose target is an absolute path, climbs out with `..`, is named through a `$`
// expression or carries a cluster, a pragma that moves the prefix, a statement
// that runs code it computes, and every object the whole database shares: users,
// groups, permissions, secrets of the deprecated object kind, resource pools and
// backups. A statement that reads outside the realm is left alone, since a read
// leaves nothing behind. An external data source or table, an async replication,
// a transfer and a streaming query reach past the realm and are refused. A
// statement the guard does not recognize is refused.

// RealmRefusal reports a statement whose effects cannot be confined to a dev
// realm.
type RealmRefusal struct {
	// Operation names what the statement does that cannot be confined.
	Operation string
}

func (e *RealmRefusal) Error() string {
	return fmt.Sprintf("YDB dev realm session rejects %s because its effects cannot be confined to the dev realm", e.Operation)
}

// OperationOrEmpty returns the operation a refusal names, and "" for a nil
// refusal, so a caller can read both outcomes the same way.
func (e *RealmRefusal) OperationOrEmpty() string {
	if e == nil {
		return ""
	}
	return e.Operation
}

func refuse(operation string) error { return &RealmRefusal{Operation: operation} }

// ConfineToRealm reads source, one or more statements, and returns a
// *RealmRefusal for the first statement whose effects cannot be confined to a
// dev realm.
func ConfineToRealm(source string) error {
	stream := reader{input: source}
	var tokens []Token
	var bodies Bodies
	for {
		token := stream.NextToken()
		switch token.Type {
		case TokenEOF:
			return confineStatement(tokens)
		case TokenWhitespace, TokenComment:
			continue
		case TokenUnknown:
			// The default syntax is the only setting that leaves the reading of
			// the text as it is. The host's guard refuses it with the others.
			if strings.TrimSpace(token.Value) == "--!syntax_v1" {
				continue
			}
		case TokenSemicolon:
			bodies.Separate()
			if bodies.Inside() {
				continue
			}
			if err := confineStatement(tokens); err != nil {
				return err
			}
			tokens = nil
			continue
		case TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case TokenOperator:
			bodies.Symbol(token.Value)
		default:
			bodies.Separate()
		}
		tokens = append(tokens, token)
	}
}

// confineStatement judges one statement.
func confineStatement(tokens []Token) error {
	if len(tokens) == 0 {
		return nil
	}
	if tokens[0].Type == TokenUnknown && strings.HasPrefix(tokens[0].Value, "--!") {
		// A translation setting changes how the rest of the text is read,
		// which the reading below cannot follow.
		return refuse("translation setting " + tokens[0].Value)
	}
	if operation := beyondTheServer(tokens); operation != "" {
		return refuse(operation)
	}
	return confineToRealm(tokens)
}

// beyondTheServer names the operation of a statement that reads from or writes
// to something outside the server, or "". An external data source is refused
// wherever the statement names one, since a table's TTL moves rows to one
// through `TO EXTERNAL DATA SOURCE`.
func beyondTheServer(tokens []Token) string {
	if containsKeywords(tokens, "EXTERNAL", "DATA", "SOURCE") {
		return "an external data source"
	}
	switch keyword(tokens[0]) {
	case "CREATE", "ALTER", "DROP":
	default:
		return ""
	}
	kind := objectKindIndex(tokens)
	switch {
	case keywordAt(tokens, kind) == "EXTERNAL" && keywordAt(tokens, kind+1) == "TABLE":
		return "an external table"
	case keywordAt(tokens, kind) == "ASYNC" && keywordAt(tokens, kind+1) == "REPLICATION":
		return "async replication"
	case keywordAt(tokens, kind) == "TRANSFER":
		return "a transfer"
	case keywordAt(tokens, kind) == "STREAMING" && keywordAt(tokens, kind+1) == "QUERY":
		return "a streaming query"
	default:
		return ""
	}
}

// objectKindIndex is the index of the word that names the kind of object a
// CREATE, ALTER or DROP statement is about, past an OR REPLACE.
func objectKindIndex(tokens []Token) int {
	if keywordAt(tokens, 1) == "OR" && keywordAt(tokens, 2) == "REPLACE" {
		return 3
	}
	return 1
}

// confineToRealm refuses a statement whose effect leaves the realm.
func confineToRealm(tokens []Token) error {
	first := keyword(tokens[0])
	switch first {
	case "SELECT", "DECLARE", "PROCESS", "REDUCE", "VALUES", "COMMIT", "ROLLBACK":
		return nil
	case "PRAGMA":
		if strings.EqualFold(pragmaName(tokens), "TablePathPrefix") {
			return refuse("PRAGMA TablePathPrefix")
		}
		return nil
	case "USE":
		return refuse("USE")
	case "DEFINE":
		if keywordAt(tokens, 1) == "SUBQUERY" {
			return nil
		}
		return refuse("DEFINE ACTION")
	case "DO", "EVALUATE":
		return refuse(first)
	case "GRANT", "REVOKE":
		return refuse(first + " permission change")
	case "BACKUP", "RESTORE":
		return refuse(first)
	case "INSERT", "UPSERT", "REPLACE":
		return checkTargetAfter(tokens, 1, "INTO")
	case "UPDATE":
		return checkTarget(tokens, 1)
	case "DELETE":
		return checkTargetAfter(tokens, 1, "FROM")
	case "BATCH":
		return confineToRealm(tokens[1:])
	case "ANALYZE":
		return checkTarget(tokens, 1)
	case "TRUNCATE":
		return checkTargetAfter(tokens, 1, "TABLE")
	case "CREATE", "ALTER", "DROP":
		return confineObject(tokens)
	}
	if strings.HasPrefix(tokens[0].Value, "$") && tokens[0].Type == TokenIdentifier {
		// A named expression or lambda defines a value and writes nothing; a
		// write that uses it is judged by its own statement.
		return nil
	}
	return refuse("unrecognized statement " + strings.ToUpper(tokens[0].Value))
}

// confineObject refuses a CREATE, ALTER or DROP whose object the whole
// database shares, or whose path leaves the realm.
func confineObject(tokens []Token) error {
	kind := objectKindIndex(tokens)
	switch keywordAt(tokens, kind) {
	case "TABLE":
		if err := checkTarget(tokens, skipExistenceGuard(tokens, kind+1)); err != nil {
			return err
		}
		return checkRenameTarget(tokens)
	case "VIEW", "SEQUENCE", "TOPIC", "SECRET":
		return checkTarget(tokens, skipExistenceGuard(tokens, kind+1))
	case "COORDINATION":
		if keywordAt(tokens, kind+1) != "NODE" {
			return refuse("unrecognized object " + strings.ToUpper(tokens[0].Value) + " COORDINATION " + strings.ToUpper(valueAt(tokens, kind+1)))
		}
		return checkTarget(tokens, kind+2)
	case "TEMP", "TEMPORARY":
		return refuse("temporary table")
	case "USER", "GROUP":
		return refuse("a " + strings.ToLower(keywordAt(tokens, kind)) + " of the whole database")
	case "OBJECT":
		// CREATE OBJECT makes the deprecated secret object, which belongs to
		// the user rather than to a path, and the other objects of the
		// database's metadata service.
		return refuse("an object of the whole database")
	case "RESOURCE":
		return refuse("a resource pool of the whole database")
	case "BACKUP":
		return refuse("a backup collection of the whole database")
	case "DATABASE":
		return refuse("a change to the whole database")
	default:
		return refuse("unrecognized object " + strings.ToUpper(tokens[0].Value) + " " + strings.ToUpper(valueAt(tokens, kind)))
	}
}

// skipExistenceGuard returns the index after an IF EXISTS or IF NOT EXISTS that
// starts at index, or index when none does.
func skipExistenceGuard(tokens []Token, index int) int {
	if keywordAt(tokens, index) != "IF" {
		return index
	}
	if keywordAt(tokens, index+1) == "NOT" {
		return index + 3
	}
	return index + 2
}

// checkRenameTarget checks the new path of `ALTER TABLE ... RENAME TO`.
// `RENAME INDEX a TO b` renames an index of the table, which is no path.
func checkRenameTarget(tokens []Token) error {
	for index := range tokens {
		if keyword(tokens[index]) == "RENAME" && keywordAt(tokens, index+1) == "TO" {
			if err := checkTarget(tokens, index+2); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTargetAfter checks the target that follows the keyword, which sits at
// index or after a modifier such as `OR IGNORE`.
func checkTargetAfter(tokens []Token, index int, word string) error {
	position := slices.IndexFunc(tokens[min(index, len(tokens)):], func(token Token) bool {
		return keyword(token) == word
	})
	if position < 0 {
		return refuse("a " + strings.ToUpper(tokens[0].Value) + " with no " + word + " the guard can read")
	}
	return checkTarget(tokens, index+position+1)
}

// checkTarget refuses the write target at index when it is not a relative path
// inside the realm.
func checkTarget(tokens []Token, index int) error {
	if index >= len(tokens) {
		return refuse("a statement whose target the guard cannot read")
	}
	target := tokens[index]
	switch {
	case target.Type != TokenIdentifier:
		return refuse("a target written as " + target.Value)
	case strings.HasPrefix(target.Value, "$"):
		return refuse("a target named through " + target.Value)
	case index+1 < len(tokens) && tokens[index+1].Type == TokenOperator && (tokens[index+1].Value == "." || tokens[index+1].Value == ":"):
		return refuse("a target in the cluster " + target.Value)
	}
	name, ok := IdentifierValue(target.Value)
	switch {
	case !ok:
		return refuse("a target the guard cannot read")
	case strings.HasPrefix(name, "/"):
		return refuse("the absolute path " + name)
	case slices.Contains(strings.Split(name, "/"), ".."):
		return refuse("the path " + name + ", which climbs out of the dev realm")
	}
	return nil
}

// pragmaName is the name a PRAGMA sets, without the namespace a dotted name
// carries: `TablePathPrefix` for `PRAGMA ydb.TablePathPrefix = ...`.
func pragmaName(tokens []Token) string {
	name := ""
	for index := 1; index < len(tokens); index++ {
		token := tokens[index]
		if token.Type != TokenIdentifier {
			break
		}
		name, _ = IdentifierValue(token.Value)
		if index+1 >= len(tokens) || tokens[index+1].Type != TokenOperator || tokens[index+1].Value != "." {
			break
		}
		index++
	}
	return name
}

// keyword is a bare word in upper case, or "" for anything else: a backticked
// identifier, a `$` name, a string or an operator is never a keyword.
func keyword(token Token) string {
	if token.Type != TokenIdentifier || token.Value == "" || token.Value[0] == '`' || token.Value[0] == '$' {
		return ""
	}
	return strings.ToUpper(token.Value)
}

// keywordAt is keyword of the token at index, or "" past the end.
func keywordAt(tokens []Token, index int) string {
	if index < 0 || index >= len(tokens) {
		return ""
	}
	return keyword(tokens[index])
}

// valueAt is the text of the token at index, or "" past the end.
func valueAt(tokens []Token, index int) string {
	if index < 0 || index >= len(tokens) {
		return ""
	}
	return tokens[index].Value
}

// containsKeywords reports whether the bare words appear in sequence.
func containsKeywords(tokens []Token, words ...string) bool {
	for start := range tokens {
		matched := true
		for offset, word := range words {
			if keywordAt(tokens, start+offset) != word {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
