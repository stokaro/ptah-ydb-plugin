package yql_test

import (
	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"strings"
	"testing"
)

func analyzedText(c *qt.C, source string) []string {
	c.Helper()
	request := &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source}}
	result, err := yql.Analyze(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
	var text []string
	for _, statement := range result.Files[0].Statements {
		text = append(text, strings.TrimSuffix(strings.TrimSpace(source[statement.Start:statement.End]), ";"))
	}
	return text
}

func TestSplitSQLStatementsForDialect_YDBCompoundBodies(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "an action and the statement that runs it",
			sql:  "DEFINE ACTION $a($x) AS SELECT $x AS v; SELECT 2 AS w; END DEFINE; DO $a(1);",
			want: []string{"DEFINE ACTION $a($x) AS SELECT $x AS v; SELECT 2 AS w; END DEFINE", "DO $a(1)"},
		},
		{
			name: "a subquery",
			sql:  "DEFINE SUBQUERY $s() AS SELECT 1 AS x; END DEFINE; SELECT * FROM $s();",
			want: []string{"DEFINE SUBQUERY $s() AS SELECT 1 AS x; END DEFINE", "SELECT * FROM $s()"},
		},
		{
			name: "EVALUATE FOR with a block",
			sql:  "EVALUATE FOR $i IN AsList(1, 2) DO BEGIN SELECT $i AS i; SELECT 0 AS z; END DO;\nSELECT 3;",
			want: []string{"EVALUATE FOR $i IN AsList(1, 2) DO BEGIN SELECT $i AS i; SELECT 0 AS z; END DO", "SELECT 3"},
		},
		{
			name: "EVALUATE IF with two blocks",
			sql:  "EVALUATE IF true DO BEGIN SELECT 1 AS a; END DO ELSE DO BEGIN SELECT 2 AS b; END DO; SELECT 3;",
			want: []string{"EVALUATE IF true DO BEGIN SELECT 1 AS a; END DO ELSE DO BEGIN SELECT 2 AS b; END DO", "SELECT 3"},
		},
		{
			name: "a plain DO block",
			sql:  "DO BEGIN SELECT 1 AS a; SELECT 2 AS b; END DO;",
			want: []string{"DO BEGIN SELECT 1 AS a; SELECT 2 AS b; END DO"},
		},
		{
			// CASE ... END inside the block closes the CASE, not the block,
			// and END DO inside the action closes the block, not the action.
			name: "a block inside an action, holding a CASE",
			sql: "DEFINE ACTION $a() AS DO BEGIN SELECT CASE WHEN true THEN 1 ELSE 2 END AS c; END DO; END DEFINE;\n" +
				"DO $a();",
			want: []string{
				"DEFINE ACTION $a() AS DO BEGIN SELECT CASE WHEN true THEN 1 ELSE 2 END AS c; END DO; END DEFINE",
				"DO $a()",
			},
		},
		{
			name: "a lambda body",
			sql:  "$f = ($x) -> { $y = $x + 1; RETURN $y; }; SELECT $f(1) AS r;",
			want: []string{"$f = ($x) -> { $y = $x + 1; RETURN $y; }", "SELECT $f(1) AS r"},
		},
		{
			name: "a lambda inside a lambda",
			sql:  "$f = ($x) -> { $g = ($y) -> { RETURN $y; }; RETURN $g($x); }; SELECT 1;",
			want: []string{"$f = ($x) -> { $g = ($y) -> { RETURN $y; }; RETURN $g($x); }", "SELECT 1"},
		},
		{
			// A comment between the two words of a closing pair is
			// whitespace to the server.
			name: "a comment inside END DEFINE",
			sql:  "DEFINE ACTION $a() AS SELECT 1; END /* x */ DEFINE; SELECT 2;",
			want: []string{"DEFINE ACTION $a() AS SELECT 1; END /* x */ DEFINE", "SELECT 2"},
		},
		{
			// END; DO is two statements, not a closing pair.
			name: "END and DO on either side of a semicolon",
			sql:  "SELECT CASE WHEN true THEN 1 ELSE 2 END; DO $a();",
			want: []string{"SELECT CASE WHEN true THEN 1 ELSE 2 END", "DO $a()"},
		},
		{
			// Inside a body too: the CASE's END and the next statement's DO
			// are separated by a semicolon, so they close nothing.
			name: "END and DO on either side of a semicolon inside a body",
			sql:  "DEFINE ACTION $b() AS SELECT CASE WHEN true THEN 1 ELSE 2 END; DO EMPTY_ACTION(); END DEFINE; DO $b();",
			want: []string{
				"DEFINE ACTION $b() AS SELECT CASE WHEN true THEN 1 ELSE 2 END; DO EMPTY_ACTION(); END DEFINE",
				"DO $b()",
			},
		},
		{
			// The CASE's END and the IF's DO are adjacent words, and they
			// close nothing.
			name: "a CASE ending right before an IF's DO, inside a body",
			sql: "DEFINE ACTION $a() AS EVALUATE IF CASE WHEN true THEN true ELSE false END DO BEGIN SELECT 1 AS x; " +
				"SELECT 2 AS y; END DO; END DEFINE; DO $a();",
			want: []string{
				"DEFINE ACTION $a() AS EVALUATE IF CASE WHEN true THEN true ELSE false END DO BEGIN SELECT 1 AS x; " +
					"SELECT 2 AS y; END DO; END DEFINE",
				"DO $a()",
			},
		},
		{
			name: "a parenthesized CASE before an IF's DO, inside a body",
			sql: "DEFINE ACTION $a() AS EVALUATE IF (CASE WHEN true THEN true ELSE false END) DO BEGIN SELECT 1 AS x; " +
				"SELECT 2 AS y; END DO; END DEFINE; DO $a();",
			want: []string{
				"DEFINE ACTION $a() AS EVALUATE IF (CASE WHEN true THEN true ELSE false END) DO BEGIN SELECT 1 AS x; " +
					"SELECT 2 AS y; END DO; END DEFINE",
				"DO $a()",
			},
		},
		{
			// After AS and after a dot, case and end are names.
			name: "keyword spellings used as names inside a body",
			sql: "DEFINE ACTION $a() AS SELECT 1 AS case, 2 AS end; SELECT t.case FROM (SELECT 3 AS case) AS t; " +
				"END DEFINE; DO $a();",
			want: []string{
				"DEFINE ACTION $a() AS SELECT 1 AS case, 2 AS end; SELECT t.case FROM (SELECT 3 AS case) AS t; " +
					"END DEFINE",
				"DO $a()",
			},
		},
		{
			name: "a field named end before an IF's DO, inside a body",
			sql: "DEFINE ACTION $a($s) AS EVALUATE IF $s.end DO BEGIN SELECT 1 AS x; SELECT 2 AS y; END DO; END DEFINE; " +
				"DO $a(AsStruct(true AS end));",
			want: []string{
				"DEFINE ACTION $a($s) AS EVALUATE IF $s.end DO BEGIN SELECT 1 AS x; SELECT 2 AS y; END DO; END DEFINE",
				"DO $a(AsStruct(true AS end))",
			},
		},
		{
			// Measured: two columns, then a second statement. A comma
			// separates DO and BEGIN, so they open no block.
			name: "DO and BEGIN as columns",
			sql:  "SELECT do, begin FROM (SELECT 1 AS do, 2 AS begin); SELECT 2 AS two;",
			want: []string{"SELECT do, begin FROM (SELECT 1 AS do, 2 AS begin)", "SELECT 2 AS two"},
		},
		{
			// Measured: the action answered both selects. A comma separates
			// END and DEFINE, so they close nothing.
			name: "END and DEFINE as columns inside an action",
			sql: "DEFINE ACTION $a() AS SELECT end, define FROM (SELECT 1 AS end, 2 AS define); SELECT 2 AS two; " +
				"END DEFINE; DO $a();",
			want: []string{
				"DEFINE ACTION $a() AS SELECT end, define FROM (SELECT 1 AS end, 2 AS define); SELECT 2 AS two; " +
					"END DEFINE",
				"DO $a()",
			},
		},
		{
			// A backticked word is a name, never the keyword.
			name: "a quoted END is a name",
			sql:  "DEFINE ACTION $a() AS SELECT 1 AS `END`; SELECT 2; END DEFINE; SELECT 3;",
			want: []string{"DEFINE ACTION $a() AS SELECT 1 AS `END`; SELECT 2; END DEFINE", "SELECT 3"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(analyzedText(c, test.sql), qt.DeepEquals, test.want)
		})
	}
}

// The literal and comment rows are read the way YDB 26.2.1.14 read them; see
// internal/lexer's YQL tests for each measurement.
func TestSplitSQLStatementsForDialect_YDBLiteralsAndComments(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "a double-quoted string holds a semicolon",
			sql:  `SELECT "a;b" AS x; SELECT 2;`,
			want: []string{`SELECT "a;b" AS x`, "SELECT 2"},
		},
		{
			name: "an escaped quote does not pair with the quote after it",
			sql:  `SELECT 'a\''; SELECT 2;`,
			want: []string{`SELECT 'a\''`, "SELECT 2"},
		},
		{
			name: "strings holding comment markers and a backtick",
			sql:  "SELECT '-- x', \"/* y\", '`'; SELECT 2;",
			want: []string{"SELECT '-- x', \"/* y\", '`'", "SELECT 2"},
		},
		{
			name: "a multiline string holds a semicolon and an escaped @@",
			sql:  "SELECT @@a;\n@@@@;b@@j; SELECT 2;",
			want: []string{"SELECT @@a;\n@@@@;b@@j", "SELECT 2"},
		},
		{
			name: "a backtick identifier holds a semicolon and an escaped backtick",
			sql:  "SELECT * FROM `dir/t;\\`x`; SELECT 2;",
			want: []string{"SELECT * FROM `dir/t;\\`x`", "SELECT 2"},
		},
		{
			name: "comments hold semicolons",
			sql:  "SELECT 1 -- a; b\n/* c; d */; SELECT 2;",
			want: []string{"SELECT 1 -- a; b\n/* c; d */", "SELECT 2"},
		},
		{
			name: "a hash does not start a comment",
			sql:  "SELECT 1 # x; SELECT 2;",
			want: []string{"SELECT 1 # x", "SELECT 2"},
		},
		{
			name: "a dollar sign does not open a dollar-quoted string",
			sql:  "SELECT $a$; SELECT $a$;",
			want: []string{"SELECT $a$", "SELECT $a$"},
		},
		{
			// The delimiter rewriter would turn the GO inside the literal
			// into a semicolon. YQL has no client delimiter, so the text is
			// split as written and the server refuses the GO it does not know.
			name: "an Atlas delimiter directive is not honored",
			sql:  "-- atlas:delimiter GO\nSELECT @@a GO b@@ GO\nSELECT 'x\\'GO' GO\n",
			want: []string{"-- atlas:delimiter GO\nSELECT @@a GO b@@ GO\nSELECT 'x\\'GO' GO"},
		},
		{
			name: "a MySQL delimiter directive is not honored",
			sql:  "DELIMITER //\nSELECT 1 //\nSELECT 2;",
			want: []string{"DELIMITER //\nSELECT 1 //\nSELECT 2"},
		},
		{
			name: "a translation setting stays with the statement it heads",
			sql:  "--!syntax_v1\nSELECT 1; SELECT 2;",
			want: []string{"--!syntax_v1\nSELECT 1", "SELECT 2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(analyzedText(c, test.sql), qt.DeepEquals, test.want)
		})
	}
}
