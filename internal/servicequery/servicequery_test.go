package servicequery_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// opensWith reports a statement that opens with word.
func opensWith(word string) func([]yql.Token) bool {
	return func(tokens []yql.Token) bool { return tokens[0].IsWord(word) }
}

func TestLocate_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name, text, opening, prefix string
	}{
		{name: "alone", text: "MARK x;", opening: "MARK X"},
		{name: "after definitions and comments", text: "-- c\n/* d */ DECLARE $a AS Int32; $b = 1; PRAGMA TablePathPrefix('/db/p'); MARK x",
			opening: "MARK X", prefix: "/db/p"},
		{name: "a later prefix wins", text: "PRAGMA TablePathPrefix('/a'); PRAGMA TablePathPrefix = \"/b\"; MARK x;", opening: "MARK X", prefix: "/b"},
		// Ported from the host's TestLocate_HappyPath.
		{name: "lower case", text: "mark x", opening: "MARK X"},
		{name: "after the translation setting", text: "--!syntax_v1\nMARK x", opening: "MARK X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			located, found, err := servicequery.Locate(tc.text, opensWith("MARK"), "test service")
			c.Assert(err, qt.IsNil)
			c.Assert(found, qt.IsTrue)
			c.Assert(servicequery.FirstWords(located.Tokens, 2), qt.Equals, tc.opening)
			c.Assert(located.PathPrefix, qt.Equals, tc.prefix)
		})
	}
}

// Text holding no such statement is not recognized and reports no error, the
// word inside a string or a comment, or after the first word, included. The
// rows after the first are ported from the host's TestLocate_NotRecognized.
func TestLocateLeavesOtherQueries(t *testing.T) {
	for _, tc := range []struct {
		name, text string
	}{
		{name: "in a string and a line comment", text: "SELECT 'MARK'; -- MARK\n"},
		{name: "empty", text: ""},
		{name: "another statement", text: "SELECT 1"},
		{name: "in a block comment", text: "/* MARK x */ SELECT 1"},
		{name: "not first", text: "SELECT MARK FROM t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			located, found, err := servicequery.Locate(tc.text, opensWith("MARK"), "test service")
			c.Assert(err, qt.IsNil)
			c.Assert(found, qt.IsFalse)
			c.Assert(located, qt.DeepEquals, servicequery.Located{})
		})
	}
}

func TestLocate_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{name: "two", text: "MARK a; MARK b;", want: "a query runs one MARK B statement, and this one holds two; put each in a query of its own"},
		{name: "not last", text: "MARK a; SELECT 1;", want: "MARK A is followed by another statement in its query, and Ptah runs it through YDB's test service, which takes it alone"},
		{name: "after a statement", text: "SELECT 1; MARK a;", want: "MARK A shares its query with SELECT 1, and Ptah runs it through YDB's test service, which takes it alone"},
		{name: "a prefix that is not a plain string", text: "PRAGMA TablePathPrefix($p); MARK a;",
			want: "MARK A follows a TablePathPrefix pragma whose path is not a plain string"},
		// Ported from the host's TestLocate_FailurePath.
		{name: "a prefix Ptah cannot read", text: "PRAGMA TablePathPrefix('/a', '/b'); MARK a",
			want: `MARK A follows a TablePathPrefix pragma Ptah cannot read; write it as PRAGMA TablePathPrefix\("/database/path"\)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			located, found, err := servicequery.Locate(tc.text, opensWith("MARK"), "test service")
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(found, qt.IsTrue)
			c.Assert(located, qt.DeepEquals, servicequery.Located{})
		})
	}
}

func TestStringValue(t *testing.T) {
	// #nosec G101 -- token is a lexer token, not a credential
	for _, tc := range []struct {
		token string
		want  string
		ok    bool
	}{
		{token: `'a\'b\\c\n\x41'`, want: "a'b\\c\nA", ok: true},
		{token: `"x"u`, want: "x", ok: true},
		{token: `'bad\q'`},
		{token: `'cut\x4'`},
		{token: `'a'b'`},
	} {
		t.Run(tc.token, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := servicequery.StringValue(yql.Token{Type: yql.TokenString, Value: tc.token})
			c.Assert(ok, qt.Equals, tc.ok)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}
