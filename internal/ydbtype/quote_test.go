package ydbtype_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// A string literal keeps a quote, a backslash and a line break as data, and
// the scanner reads it back as the text it was written from. Ported from the
// host's ExampleStringLiteral.
func TestStringLiteral_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"a quote, a backslash and a newline", "it's\\ready\n", `'it\'s\\ready\n'`},
		{"a carriage return and a tab", "a\rb\tc", `'a\rb\tc'`},
		{"a control character", "a\x01b", `'a\x01b'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			literal := ydbtype.StringLiteral(test.value)
			c.Assert(literal, qt.Equals, test.want)
			read, ok := yql.StringValue(literal)
			c.Assert(ok, qt.IsTrue)
			c.Assert(read, qt.Equals, test.value)
		})
	}
}

// A quoted name is one token to YQL's scanner, whatever it holds, and the
// scanner reads the token back as the name: a backtick or a backslash cannot
// end it early, so text after them stays inside the name. Ported from the
// host's TestQuoteYQLIdentifier_TheLexerReadsOneName and the round trip of
// TestReadName_HappyPath.
func TestQuoteIdentifier_TheScannerReadsOneName(t *testing.T) {
	for _, name := range []string{"items", "a`b", `a\`, "a\\`; DROP TABLE x; --", "``", "c`d", `a\b`, "e\\`f", "dir/sub/t.v1", "line\nnext", "Ärger"} {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			quoted := ydbtype.QuoteIdentifier(name)
			tokens := yql.Tokens(quoted + " AS x")
			c.Assert(tokens[0].Type, qt.Equals, yql.TokenIdentifier)
			c.Assert(tokens[0].Value, qt.Equals, quoted)
			read, ok := yql.IdentifierValue(tokens[0].Value)
			c.Assert(ok, qt.IsTrue)
			c.Assert(read, qt.Equals, name)
		})
	}
}
