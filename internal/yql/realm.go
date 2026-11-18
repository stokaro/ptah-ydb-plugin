package yql

import (
	"errors"
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
//
// A run whose operator trusts the SQL it executes, with the privileges of the
// dev connection, lifts the refusals whose only reason is that the effects
// cannot be confined to the realm: code a statement computes (DO, EVALUATE,
// DEFINE ACTION), a permission change, a user or a group, a backup, a restore
// or a backup collection, a write whose target is outside the realm or cannot
// be read, and a statement that reaches past the server. An object it creates
// outside the realm stays there: the realm's cleanup removes the realm and
// nothing else. Trust keeps every refusal that protects the session or its
// cleanup: a translation setting, a pragma that moves the prefix, USE, a
// temporary table, a resource pool, an object of the database's metadata
// service, a change to the whole database, and a statement or an object kind
// the guard does not recognize, since it cannot tell what that one changes.
// Trust lifts the code a statement computes and still reads each statement of
// a DO, DEFINE ACTION or DEFINE SUBQUERY body, so a refusal it keeps stays
// refused there too.

// RealmRefusal reports a statement whose effects cannot be confined to a dev
// realm.
type RealmRefusal struct {
	// Operation names what the statement does that cannot be confined.
	Operation string
	// Kept reports a refusal trust does not lift: the statement would change
	// the session, the database's configuration, or something the guard
	// cannot read.
	Kept bool
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

// refuse is the refusal of a statement whose effects cannot be confined to
// the realm, which trust lifts.
func refuse(operation string) error { return &RealmRefusal{Operation: operation} }

// keep is the refusal of a statement trust does not lift.
func keep(operation string) error { return &RealmRefusal{Operation: operation, Kept: true} }

// strict returns the refusal it is given: the policy of a run that does not
// trust its SQL.
func strict(err error) error { return err }

// trusted returns nil for a refusal trust lifts, and any other error as it is:
// the policy of a run whose operator trusts the SQL it executes.
func trusted(err error) error {
	if refusal, ok := errors.AsType[*RealmRefusal](err); ok && !refusal.Kept {
		return nil
	}
	return err
}

// ConfineToRealm reads source, one or more statements, and returns a
// *RealmRefusal for the first statement whose effects cannot be confined to a
// dev realm.
func ConfineToRealm(source string) error {
	return confineSource(source, strict)
}

// ConfineTrustedToRealm is [ConfineToRealm] for a session whose run trusts the
// SQL it executes: it returns only a refusal trust keeps.
func ConfineTrustedToRealm(source string) error {
	return confineSource(source, trusted)
}

// confineSource judges each statement of source and returns the first error
// policy keeps.
func confineSource(source string, policy func(error) error) error {
	stream := reader{input: source}
	var tokens []Token
	var cuts []int
	var bodies Bodies
	for {
		token := stream.NextToken()
		switch token.Type {
		case TokenEOF:
			return policy(confineWithBodies(tokens, cuts, confineStatement))
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
				cuts = append(cuts, len(tokens))
				continue
			}
			if err := policy(confineWithBodies(tokens, cuts, confineStatement)); err != nil {
				return err
			}
			tokens, cuts = nil, nil
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

// confineWithBodies is judge(tokens), unless that verdict is one trust lifts
// and a statement inside one of the statement's DO, DEFINE ACTION or DEFINE
// SUBQUERY bodies is refused in a way trust keeps. Trust lifts the code a
// statement computes, not what each statement of that code would keep
// refused on its own: without this, DO BEGIN PRAGMA TablePathPrefix(...); ...
// END DO moves the prefix under trust although the pragma alone stays
// refused. cuts are the indexes in tokens where a semicolon inside a body
// ended a statement.
func confineWithBodies(tokens []Token, cuts []int, judge func([]Token) error) error {
	verdict := judge(tokens)
	if refusal, ok := errors.AsType[*RealmRefusal](verdict); ok && refusal.Kept {
		return verdict
	}
	if kept := keptInBodies(tokens, cuts, judge); kept != nil {
		return kept
	}
	return verdict
}

// keptInBodies returns the first refusal trust keeps among the statements of
// the DO, DEFINE ACTION and DEFINE SUBQUERY bodies in one statement's tokens,
// as judge reads each, or nil. cuts are the indexes in tokens where a
// semicolon inside a body ended a statement.
func keptInBodies(tokens []Token, cuts []int, judge func([]Token) error) error {
	reader := bodyStatements{tokens: tokens, judge: judge}
	next := 0
	for index := range tokens {
		for ; next < len(cuts) && cuts[next] == index; next++ {
			if err := reader.separate(index); err != nil {
				return err
			}
		}
		if err := reader.read(index); err != nil {
			return err
		}
	}
	// A body the text never closes ends with the text.
	for ; len(reader.starts) > 0; reader.starts = reader.starts[:len(reader.starts)-1] {
		if err := reader.kept(len(tokens)); err != nil {
			return err
		}
	}
	return nil
}

// bodyStatements reads one statement's tokens again to find the statements of
// its bodies. A body statement starts after DO BEGIN, after the AS that ends a
// DEFINE header, and after a semicolon in the body, and ends at the next such
// semicolon or at the END that closes the body. A lambda's braces hold
// expressions and a RETURN, which no statement judgment reads, so a semicolon
// between them ends nothing here.
type bodyStatements struct {
	tokens []Token
	judge  func([]Token) error
	bodies Bodies
	// starts holds, for each open body, the index where its current statement
	// began, or -1 while a DEFINE header has not reached its AS yet.
	starts []int
}

// kept returns the refusal trust keeps of the innermost body's statement that
// ends before end, or nil.
func (r *bodyStatements) kept(end int) error {
	if len(r.starts) == 0 {
		return nil
	}
	start := r.starts[len(r.starts)-1]
	if start < 0 || start >= end {
		return nil
	}
	if refusal, ok := errors.AsType[*RealmRefusal](r.judge(r.tokens[start:end])); ok && refusal.Kept {
		return refusal
	}
	return nil
}

// separate reads a semicolon in a body, right before tokens[index].
func (r *bodyStatements) separate(index int) error {
	r.bodies.Separate()
	if len(r.starts) == 0 || r.bodies.braces > 0 {
		return nil
	}
	if err := r.kept(index); err != nil {
		return err
	}
	r.starts[len(r.starts)-1] = index
	return nil
}

// read reads tokens[index], which opens a body, closes one, ends a DEFINE
// header, or continues the statement being read.
func (r *bodyStatements) read(index int) error {
	token := r.tokens[index]
	blocks, word := r.bodies.blocks, keyword(token)
	switch token.Type {
	case TokenIdentifier:
		r.bodies.Word(strings.ToUpper(token.Value))
	case TokenOperator:
		r.bodies.Symbol(token.Value)
	default:
		r.bodies.Separate()
	}
	switch {
	case r.bodies.blocks > blocks && word == "BEGIN":
		r.starts = append(r.starts, index+1)
	case r.bodies.blocks > blocks:
		r.starts = append(r.starts, -1)
	case r.bodies.blocks < blocks && len(r.starts) > 0:
		// The END before this DO or DEFINE closes the body.
		err := r.kept(index - 1)
		r.starts = r.starts[:len(r.starts)-1]
		return err
	case word == "AS" && len(r.starts) > 0 && r.starts[len(r.starts)-1] < 0:
		r.starts[len(r.starts)-1] = index + 1
	}
	return nil
}

// confineStatement judges one statement.
func confineStatement(tokens []Token) error {
	if len(tokens) == 0 {
		return nil
	}
	if tokens[0].Type == TokenUnknown && strings.HasPrefix(tokens[0].Value, "--!") {
		// A translation setting changes how the rest of the text is read,
		// which the reading below cannot follow.
		return keep("translation setting " + tokens[0].Value)
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
			return keep("PRAGMA TablePathPrefix")
		}
		return nil
	case "USE":
		return keep("USE")
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
	case "COMMENT":
		return confineComment(tokens)
	}
	if strings.HasPrefix(tokens[0].Value, "$") && tokens[0].Type == TokenIdentifier {
		// A named expression or lambda defines a value and writes nothing; a
		// write that uses it is judged by its own statement.
		return nil
	}
	return keep("unrecognized statement " + strings.ToUpper(tokens[0].Value))
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
			return keep("unrecognized object " + strings.ToUpper(tokens[0].Value) + " COORDINATION " + strings.ToUpper(valueAt(tokens, kind+1)))
		}
		return checkTarget(tokens, kind+2)
	case "TEMP", "TEMPORARY":
		return keep("temporary table")
	case "USER", "GROUP":
		return refuse("a " + strings.ToLower(keywordAt(tokens, kind)) + " of the whole database")
	case "OBJECT":
		// CREATE OBJECT makes the deprecated secret object, which belongs to
		// the user rather than to a path, and the other objects of the
		// database's metadata service.
		return keep("an object of the whole database")
	case "RESOURCE":
		return keep("a resource pool of the whole database")
	case "BACKUP":
		return refuse("a backup collection of the whole database")
	case "DATABASE":
		return keep("a change to the whole database")
	default:
		return keep("unrecognized object " + strings.ToUpper(tokens[0].Value) + " " + strings.ToUpper(valueAt(tokens, kind)))
	}
}

// confineComment refuses one of Ptah's comment statements whose table or
// view is not a relative path inside the realm: `COMMENT ON TABLE` and `VIEW`
// name it first, `COLUMN` names it before the dot and the column, and `INDEX`
// names it after ON. The statement sets an attribute of that object alone.
func confineComment(tokens []Token) error {
	switch {
	case keywordAt(tokens, 1) != "ON":
		return keep("unrecognized statement COMMENT")
	case keywordAt(tokens, 2) == "TABLE", keywordAt(tokens, 2) == "VIEW":
		return checkTarget(tokens, 3)
	case keywordAt(tokens, 2) == "COLUMN" && len(tokens) > 3:
		return checkTarget(tokens[:4], 3)
	case keywordAt(tokens, 2) == "INDEX":
		return checkTargetAfter(tokens, 4, "ON")
	default:
		return keep("unrecognized object COMMENT ON " + strings.ToUpper(valueAt(tokens, 2)))
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
		return refuse("the path " + name + ", which climbs out of the dev database")
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

// GuardReplay judges a statement a host replays into a dev database, as the
// host's YDB replay guard (internal/devclean/ydbreplay) does, and returns a
// *RealmRefusal for the first statement of the text whose effects cannot be
// confined to the dev database the session selected. A translation setting
// changes how the rest of the text is read and is refused, --!syntax_v1
// included, as the host refuses it.
func GuardReplay(source string) error {
	return guardReplay(source, confineToRealm, strict)
}

// GuardServerReplay is [GuardReplay] for a run that owns the whole server, the
// operator's declaration or a server the run provisioned: there only what
// reaches past the server stays refused, an external data source or table, an
// async replication, a transfer and a streaming query.
func GuardServerReplay(source string) error {
	return guardReplay(source, ownedServer, strict)
}

// GuardTrustedReplay is [GuardReplay] for a run whose operator trusts the SQL
// it executes: it returns only a refusal trust keeps. The scope is still the
// dev database, and a statement trust lets through that writes outside it
// leaves an object the cleanup does not remove.
func GuardTrustedReplay(source string) error {
	return guardReplay(source, confineToRealm, trusted)
}

// GuardTrustedServerReplay is [GuardServerReplay] for a run whose operator
// trusts the SQL it executes: what reaches past the server is accepted too,
// and a translation setting stays refused.
func GuardTrustedServerReplay(source string) error {
	return guardReplay(source, ownedServer, trusted)
}

// ownedServer confines nothing: a run that owns the server may change all of
// it.
func ownedServer([]Token) error { return nil }

// guardReplay judges each statement of source: a translation setting and an
// operation past the server are refused, confine judges the rest, and policy
// decides which refusals stand.
func guardReplay(source string, confine func([]Token) error, policy func(error) error) error {
	stream := reader{input: source}
	var tokens []Token
	var cuts []int
	var bodies Bodies
	statement := func(tokens []Token) error {
		if operation := beyondTheServer(tokens); operation != "" {
			return refuse(operation)
		}
		return confine(tokens)
	}
	judge := func() error {
		if len(tokens) == 0 {
			return nil
		}
		if tokens[0].Type == TokenUnknown && strings.HasPrefix(tokens[0].Value, "--!") {
			return keep("translation setting " + tokens[0].Value)
		}
		return policy(confineWithBodies(tokens, cuts, statement))
	}
	for {
		token := stream.NextToken()
		switch token.Type {
		case TokenEOF:
			return judge()
		case TokenWhitespace, TokenComment:
			continue
		case TokenSemicolon:
			bodies.Separate()
			if bodies.Inside() {
				cuts = append(cuts, len(tokens))
				continue
			}
			if err := judge(); err != nil {
				return err
			}
			tokens, cuts = nil, nil
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
