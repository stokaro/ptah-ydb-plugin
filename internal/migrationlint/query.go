package migrationlint

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// YDB cannot run an arbitrary script as one query. A scheme statement is
// refused inside a transaction and cannot share a query with a data
// statement, so the migrator makes each scheme statement a query of its own,
// and so is a block or an action call that runs one. Such a block or call that
// also reads or writes a table is refused whole. The reader here classifies
// the statements of one text in order, as the host's internal/yqlquery does,
// remembering the actions the text defines for the calls after them.

// schemeVerbs are the first keywords of a YQL scheme statement. COMMENT is
// Ptah's own statement, which runs as a change of a table's attributes.
var schemeVerbs = []string{
	"CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "TRUNCATE", "ANALYZE", "BACKUP", "RESTORE", "COMMENT",
}

// definitionVerbs are the first keywords of a statement that defines a name or
// a setting for the rest of its query.
var definitionVerbs = []string{"PRAGMA", "DECLARE", "DEFINE", "IMPORT"}

// writeVerbs are the first keywords of a statement that writes rows.
var writeVerbs = []string{"INSERT", "UPSERT", "REPLACE", "UPDATE", "DELETE"}

// queryReader reads the statements of one YQL text in order. The zero value
// reads a text from its start.
type queryReader struct {
	// actions are what the actions the text defined so far run, by name.
	actions map[string]body
}

// mixed reports whether YDB refuses statement for running, as one query, a
// scheme statement and a statement that reads or writes a table. Only a block
// or an action call can. A DEFINE ACTION statement is recorded for the calls
// after it and is not run itself.
//
// Measured on YDB 26.2.1.14 and 25.1.4.7, each of these is refused whole with
// `Queries with mixed data and scheme operations are not supported`:
//
//	DO BEGIN UPSERT INTO t ...; CREATE TABLE m ...; END DO
//	DO BEGIN SELECT * FROM t; CREATE TABLE m ...; END DO
//	DEFINE ACTION $a() AS UPSERT INTO t ...; CREATE TABLE m ...; END DEFINE; DO $a()
//	EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t ...; DROP TABLE m; END DO
//
// A statement that reads no table does not count: `DO BEGIN SELECT 1; CREATE
// TABLE m ...; END DO` creates m.
func (r *queryReader) mixed(statement string) bool {
	return r.read(statement).mixed
}

// shape is how one statement runs.
type shape struct {
	scheme bool
	mixed  bool
}

// read classifies a statement. A definition of an action records what running
// the action runs.
func (r *queryReader) read(statement string) shape {
	tokens := queryTokens(statement)
	if len(tokens) == 0 {
		return shape{}
	}
	first := tokens[0]
	switch {
	case isNamedExpression(first):
		return shape{}
	case first.IsWord("DEFINE"):
		r.defineAction(tokens)
		return shape{}
	case slices.ContainsFunc(definitionVerbs, first.IsWord):
		return shape{}
	case first.IsWord("DO") || first.IsWord("EVALUATE"):
		runs := r.blockBody(tokens)
		return shape{scheme: runs.scheme, mixed: runs.scheme && runs.tables}
	default:
		return shape{scheme: startBody(tokens).scheme}
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

func (b body) union(other body) body {
	return body{scheme: b.scheme || other.scheme, batch: b.batch || other.batch, tables: b.tables || other.tables}
}

// startBody is what the statement tokens start runs.
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
		return body{tables: len(yqlddl.TablesRead(tokens)) > 0}
	}
}

// blockBody is what the statements a block or an action body holds run. A
// statement starts the body, follows a semicolon, or follows BEGIN; an action
// is run by the name that follows DO, and runs what its definition recorded.
func (r *queryReader) blockBody(tokens []yql.Token) body {
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
func (r *queryReader) defineAction(tokens []yql.Token) {
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

// queryTokens returns the tokens of statement that are not whitespace or
// comments. A translation setting is kept, as the host's reader keeps it.
func queryTokens(statement string) []yql.Token {
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
