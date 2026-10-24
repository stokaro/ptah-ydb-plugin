package yqlquery

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// definition classifies a statement that defines something for the
// statements after it in the same query.
const definition Kind = 0

// schemeVerbs are the first keywords of a YQL scheme statement. COMMENT is
// Ptah's own statement, which runs as a change of a table's attributes.
var schemeVerbs = []string{
	"CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "TRUNCATE", "ANALYZE", "BACKUP", "RESTORE", "COMMENT",
}

// definitionVerbs are the first keywords of a statement that defines a name or
// a setting for the rest of its query.
var definitionVerbs = []string{"PRAGMA", "DECLARE", "DEFINE", "IMPORT"}

// Defines reports whether token is the first keyword of a statement that
// defines a name or a setting for the rest of its query: PRAGMA, DECLARE,
// DEFINE or IMPORT.
func Defines(token yql.Token) bool {
	return slices.ContainsFunc(definitionVerbs, token.IsWord)
}

// writeVerbs are the first keywords of a statement that writes rows.
var writeVerbs = []string{"INSERT", "UPSERT", "REPLACE", "UPDATE", "DELETE"}

// Reader reads the statements of one YQL text in order and says how YDB runs
// each, remembering the actions the text defines for the statements that run
// them. [Split] reads a text through it, so a caller that walks the same
// statements one at a time gets the answers the split acted on. The zero
// value reads a text from its start.
type Reader struct {
	// actions are what the actions the text defined so far run, by name.
	actions map[string]body
}

// Mixed reports whether YDB refuses statement for running, as one query, a
// scheme statement and a statement that reads or writes a table. Only a block
// or an action call can. A DEFINE ACTION statement is recorded for the calls
// after it and is not run itself.
//
// Measured by the host on YDB 26.2.1.14 and 25.1.4.7, each of these is
// refused whole with `Queries with mixed data and scheme operations are not
// supported`:
//
//	DO BEGIN UPSERT INTO t ...; CREATE TABLE m ...; END DO
//	DO BEGIN SELECT * FROM t; CREATE TABLE m ...; END DO
//	DEFINE ACTION $a() AS UPSERT INTO t ...; CREATE TABLE m ...; END DEFINE; DO $a()
//	EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t ...; DROP TABLE m; END DO
//
// A statement that reads no table does not count: `DO BEGIN SELECT 1; CREATE
// TABLE m ...; END DO` creates m.
func (r *Reader) Mixed(statement string) bool {
	return r.read(statement).mixed
}

// Scheme reports whether statement runs as a scheme query: a scheme statement,
// or a block or an action call that runs one.
func (r *Reader) Scheme(statement string) bool {
	return r.read(statement).kind == Scheme
}

// shape is how one statement runs.
type shape struct {
	kind  Kind
	mixed bool
}

// read classifies a statement. A definition of an action records what running
// the action runs.
func (r *Reader) read(statement string) shape {
	tokens := significantTokens(statement)
	if len(tokens) == 0 {
		return shape{kind: Data}
	}
	first := tokens[0]
	switch {
	case isNamedExpression(first):
		// A statement that starts with a named expression assigns it; YQL
		// has no other statement that opens with one.
		return shape{kind: definition}
	case first.IsWord("DEFINE"):
		r.defineAction(tokens)
		return shape{kind: definition}
	case slices.ContainsFunc(definitionVerbs, first.IsWord):
		return shape{kind: definition}
	case first.IsWord("DO") || first.IsWord("EVALUATE"):
		runs := r.blockBody(tokens)
		return shape{kind: runs.kind(), mixed: runs.mixed()}
	default:
		return shape{kind: startBody(tokens).kind()}
	}
}

// body is what a block, an action body or a single statement runs.
type body struct {
	// scheme is a scheme statement.
	scheme bool
	// batch is a BATCH statement.
	batch bool
	// tables is a statement that reads or writes a table.
	tables bool
}

// kind is the kind of query a body runs as: a scheme statement decides it,
// then a BATCH statement, and anything else is data.
func (b body) kind() Kind {
	switch {
	case b.scheme:
		return Scheme
	case b.batch:
		return Batch
	default:
		return Data
	}
}

// mixed reports whether a body runs a scheme statement and a statement that
// reads or writes a table in one query.
func (b body) mixed() bool {
	return b.scheme && b.tables
}

func (b body) union(other body) body {
	return body{scheme: b.scheme || other.scheme, batch: b.batch || other.batch, tables: b.tables || other.tables}
}

// startBody is what the statement tokens start runs: a scheme statement, a
// BATCH statement, a statement that reads or writes a table, or none of them.
func startBody(tokens []yql.Token) body {
	first := tokens[0]
	objectWrite := len(tokens) > 1 && tokens[1].IsWord("OBJECT")
	switch {
	case slices.ContainsFunc(schemeVerbs, first.IsWord):
		return body{scheme: true}
	case (first.IsWord("UPSERT") || first.IsWord("REPLACE")) && objectWrite:
		// UPSERT OBJECT writes a scheme object, not a row.
		return body{scheme: true}
	case first.IsWord("BATCH"):
		return body{batch: true}
	case slices.ContainsFunc(writeVerbs, first.IsWord):
		return body{tables: true}
	default:
		// A statement reads a table when it names one after FROM or JOIN.
		return body{tables: len(yqlddl.TablesRead(tokens)) > 0}
	}
}

// blockBody is what the statements a block or an action body holds run. A
// statement starts the body, follows a semicolon, or follows BEGIN; an action
// is run by the name that follows DO, and runs what its definition recorded.
// An action the text did not define runs nothing known.
func (r *Reader) blockBody(tokens []yql.Token) body {
	var runs body
	starts := true
	for i, token := range tokens {
		if starts {
			runs = runs.union(startBody(tokens[i:]))
		}
		starts = token.Type == yql.TokenSemicolon || token.IsWord("BEGIN")
		if i > 0 && tokens[i-1].IsWord("DO") && isNamedExpression(token) {
			runs = runs.union(r.actions[token.Value])
		}
	}
	return runs
}

// defineAction records what the body of a DEFINE ACTION statement runs. The
// body starts after the AS that ends the parameter list.
func (r *Reader) defineAction(tokens []yql.Token) {
	if len(tokens) < 3 || !tokens[1].IsWord("ACTION") || !isNamedExpression(tokens[2]) {
		return
	}
	for i := 3; i < len(tokens); i++ {
		if tokens[i].IsWord("AS") {
			if r.actions == nil {
				r.actions = make(map[string]body)
			}
			r.actions[tokens[2].Value] = r.blockBody(tokens[i+1:])
			return
		}
	}
}

// isNamedExpression reports whether token is a `$name`.
func isNamedExpression(token yql.Token) bool {
	return token.Type == yql.TokenIdentifier && strings.HasPrefix(token.Value, "$")
}

// significantTokens returns the tokens of statement that are not whitespace or
// comments. A translation setting is kept, as the host's reader keeps it.
func significantTokens(statement string) []yql.Token {
	var tokens []yql.Token
	for _, token := range yql.Tokens(statement) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment:
			continue
		default:
			tokens = append(tokens, token)
		}
	}
	return tokens
}
