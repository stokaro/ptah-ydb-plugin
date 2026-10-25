package migrationlint

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// statementFacts are the facts of contract 25.1 that say what a statement is,
// which table it creates without a primary key, and which capabilities it
// needs. A host reads them in place of a grammar it does not have: `ptah sql
// lint` judges a YDB source by them when this provider serves the engine.
var statementFacts = []protocol.LintFact{
	protocol.LintFact_LINT_FACT_STATEMENT_KIND, protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY,
	protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY,
}

// expressionKind names a statement that starts with no word, such as a
// literal or a parenthesized query.
const expressionKind = "expression"

// namedExpressionKind names a statement that assigns a named expression.
const namedExpressionKind = "named expression"

// describe returns the statement facts of one statement: its kind, a table it
// creates without a primary key, then the capabilities it needs, in the order
// [yqlddl.Statement.Requirements] lists them.
//
// The reading is the one the Ptah host's built-in YQL path of `ptah sql lint`
// makes (internal/sqllint, lintYQL), so a host that judges a source by these
// facts reports what that path reports: a CREATE, ALTER or DROP is a schema
// statement, named by its first two words unless it is a CREATE TABLE or an
// ALTER TABLE, and any other statement changes no schema object and is named
// by its first word. GRANT and REVOKE are among those, as they are there.
//
// The built-in path skips a statement that starts with no word, and so
// reports nothing about it; here it is an expression the linter does not
// lint, so the file cannot read as clean while holding a statement nothing
// examined. The built-in path also skips the first statement of a file that
// opens with a UTF-8 byte order mark, which the splitter here leaves out of
// the statement. A table written as a named expression keeps the name as
// written, where the built-in path names it with an empty string.
func describe(stmt *Statement) []Effect {
	keyword := firstKeyword(stmt.SQL)
	switch keyword {
	case "":
		return []Effect{kindEffect(protocol.LintStatementClass_LINT_STATEMENT_CLASS_OTHER, expressionKind, "")}
	case "CREATE", "ALTER", "DROP":
	default:
		name := keyword
		if strings.HasPrefix(keyword, "$") {
			name = namedExpressionKind
		}
		return []Effect{kindEffect(protocol.LintStatementClass_LINT_STATEMENT_CLASS_OTHER, name, "")}
	}
	read := yqlddl.Read(stmt.SQL)
	switch read.Kind {
	case yqlddl.CreateTable:
		table := writtenName(read, stmt)
		effects := []Effect{kindEffect(protocol.LintStatementClass_LINT_STATEMENT_CLASS_CREATE_TABLE, "CREATE TABLE", table)}
		if !read.PrimaryKey && table != "" {
			effects = append(effects, Effect{Fact: protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY, Table: table})
		}
		return append(effects, requirements(read, table)...)
	case yqlddl.AlterTable:
		table := writtenName(read, stmt)
		effects := []Effect{kindEffect(protocol.LintStatementClass_LINT_STATEMENT_CLASS_ALTER_TABLE, "ALTER TABLE", table)}
		return append(effects, requirements(read, table)...)
	default:
		return []Effect{kindEffect(protocol.LintStatementClass_LINT_STATEMENT_CLASS_SCHEMA, schemaKind(stmt.SQL), read.Name)}
	}
}

func kindEffect(class protocol.LintStatementClass, kind, object string) Effect {
	return Effect{Fact: protocol.LintFact_LINT_FACT_STATEMENT_KIND, Class: class, Kind: kind, Object: object}
}

// requirements are the capabilities a CREATE TABLE or an ALTER TABLE needs,
// each named by the part of the statement that needs it: an index with the
// method an inline one uses or the clause that adds it, or a column with the
// clause that adds it.
func requirements(read yqlddl.Statement, table string) []Effect {
	var effects []Effect
	for _, requirement := range read.Requirements() {
		effect := Effect{Fact: protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY, Capability: string(requirement.Capability), Table: table}
		switch {
		case requirement.Inline:
			index := read.Indexes[requirement.Action]
			effect.Kind, effect.Object, effect.Clause = "INDEX", index.Name, "USING "+index.Method
		case read.Actions[requirement.Action].Kind == yqlddl.AddColumn:
			effect.Kind, effect.Object, effect.Clause = "COLUMN", read.Actions[requirement.Action].Column.Name, "ADD COLUMN"
		default:
			effect.Kind, effect.Object, effect.Clause = "INDEX", read.Actions[requirement.Action].Index.Name, "ADD INDEX"
		}
		effects = append(effects, effect)
	}
	return effects
}

// firstKeyword is the statement's first word in upper case, or the empty
// string when the statement starts with something else: a string, a quoted
// name or punctuation. A number is a word, as the host's YQL lexer reads one.
// Comments and translation settings are skipped, as that lexer skips them.
func firstKeyword(sql string) string {
	for _, token := range yql.Tokens(strings.TrimPrefix(sql, "\uFEFF")) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment, yql.TokenUnknown:
			continue
		case yql.TokenIdentifier:
			if bareWord(token.Value) {
				return strings.ToUpper(token.Value)
			}
		}
		return ""
	}
	return ""
}

// bareWord reports a word the host's lexer reads as an identifier: not a
// quoted name, which this package's lexer reads as an identifier too.
func bareWord(value string) bool {
	return value != "" && !strings.HasPrefix(value, "`")
}

// schemaKind names a schema statement by its first two words, upper case, as
// the host's SQL004 lists the statements no rule examined. It stops at the
// first token that is not a word.
func schemaKind(sql string) string {
	var words []string
	for _, token := range yql.Tokens(strings.TrimPrefix(sql, "\uFEFF")) {
		if token.Type == yql.TokenWhitespace || token.Type == yql.TokenComment || token.Type == yql.TokenUnknown {
			continue
		}
		if len(words) == 2 || token.Type != yql.TokenIdentifier || !bareWord(token.Value) {
			break
		}
		words = append(words, strings.ToUpper(token.Value))
	}
	return strings.Join(words, " ")
}

// writtenName is the table a CREATE TABLE or an ALTER TABLE names: the path
// yqlddl reads, or, for a name written as a named expression, which yqlddl
// leaves empty, the word as written. A statement that names nothing has an
// empty name.
func writtenName(read yqlddl.Statement, stmt *Statement) string {
	if read.Name != "" {
		return read.Name
	}
	j := 2
	for j < len(stmt.Words) && slices.Contains([]string{"IF", "NOT", "EXISTS"}, stmt.Words[j]) {
		j++
	}
	if j < len(stmt.Words) && strings.HasPrefix(stmt.Words[j], "$") {
		return sourceWordAt(stmt.Words, stmt.SourceWords, j)
	}
	return ""
}
