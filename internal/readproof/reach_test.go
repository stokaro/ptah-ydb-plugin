package readproof_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/stokaro/ptah-ydb-plugin/internal/readproof"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A read that calls a function reaching files, secrets, code or PostgreSQL
// is refused, naming the function, wherever the call stands, a join condition
// and a backticked name included; a backticked module is refused by the
// colon that calls it. Ported from the host's TestScan_YDB_RefusesWhatReachesOutside.
func TestReadSourcesRefusesWhatReachesOutside(t *testing.T) {
	for _, test := range []struct {
		name, text, want string
	}{
		{"a secret", `SELECT SecureParam("token:default")`, "SECUREPARAM"},
		{"a file listing", `SELECT ListLength(Files("dir"))`, "FILES"},
		{"a folder listing", `SELECT * FROM FOLDER("dir")`, "FOLDER"},
		{"a file read in a join condition", "SELECT * FROM `t` AS a JOIN `t` AS b ON FileContent(\"secret\") = a.v", "FILECONTENT"},
		{"evaluated code", `SELECT EvaluateCode(QuoteCode(1))`, "EVALUATECODE"},
		{"evaluated code in a join condition", "SELECT * FROM `t` AS a JOIN `t` AS b ON EvaluateCode(QuoteCode(1)) = a.v", "EVALUATECODE"},
		{"any Evaluate builtin", `SELECT EVALUATEEXPR(1)`, "EVALUATEEXPR"},
		{"any Code builtin", `SELECT FuncCode("Int32")`, "FUNCCODE"},
		{"a UDF by name", `SELECT Udf("String.Strip")(" a ")`, "UDF"},
		{"a backticked builtin", "SELECT `QuoteCode`(1)", "QUOTECODE"},
		{"a side effect marker", `SELECT WithSideEffects(1)`, "WITHSIDEEFFECTS"},
		{"a PostgreSQL range function", `SELECT PgRangeCall('generate_series', 1, 2)`, "PGRANGECALL"},
		{"any Pg builtin", `SELECT pgconst(1, int4)`, "PGCONST"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := readproof.ReadSources(test.text)
			c.Assert(err, qt.ErrorMatches, "the query calls "+test.want+", whose effects cannot be proved local to the database")
			c.Assert(got, qt.DeepEquals, readproof.Reads{})
		})
	}
	c := testcheck.New(t)
	got, err := readproof.ReadSources("SELECT `String`::Contains(\"abc\", \"b\")")
	c.Assert(err, qt.ErrorMatches, ".*a colon inside it may name a cluster")
	c.Assert(got, qt.DeepEquals, readproof.Reads{})
}

// Colons and PRAGMA inside a string, a translation setting written as a
// comment after the head, a column named like a file function and an IN
// list are an ordinary read. Ported from the host's
// TestScan_YDB_LeavesOrdinaryReadsAlone.
func TestReadSourcesLeavesOrdinaryReadsAlone(t *testing.T) {
	for _, test := range []struct {
		name, text string
		sources    []string
	}{
		{"colons in a string", `SELECT "a::b" AS x`, nil},
		{"PRAGMA inside a string", `SELECT "PRAGMA File" AS x`, nil},
		{"a bang comment after the head", "SELECT 1 --!ansi_lexer", nil},
		{"a column named like a file function", "SELECT 1 AS files, zipcode FROM `t`", []string{"t"}},
		{"an IN list", "SELECT * FROM `t` AS a WHERE a.id IN (1, 2)", []string{"t"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(readproof.ReadOnly(test.text), qt.IsNil)
			got, err := readproof.ReadSources(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(got.Sources, qt.CmpEquals(cmpopts.EquateEmpty()), test.sources)
		})
	}
}
