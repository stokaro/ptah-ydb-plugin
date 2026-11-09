package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func scanned(kind protocol.SQLToken_Kind, end uint32) *protocol.SQLToken {
	return &protocol.SQLToken{Kind: kind, End: end}
}

func scannedValue(kind protocol.SQLToken_Kind, end uint32, value string) *protocol.SQLToken {
	return &protocol.SQLToken{Kind: kind, End: end, Value: new(value)}
}

// A scan returns YQL's tokens with the values YDB reads: both quote styles and
// @@ delimit strings, a backticked name is a path, and a --! comment at the
// head of a text is a setting the server reads. The reply is one the SDK
// accepts for the engine.
func TestScanSQL_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		head bool
		want []*protocol.SQLToken
	}{
		{"a statement", "SELECT \"x\"u, @@it's@@ FROM `shop/items`;", true, []*protocol.SQLToken{
			scannedValue(protocol.SQLToken_IDENTIFIER, 6, "SELECT"), scanned(protocol.SQLToken_WHITESPACE, 7),
			scannedValue(protocol.SQLToken_STRING, 11, "x"), scanned(protocol.SQLToken_OPERATOR, 12), scanned(protocol.SQLToken_WHITESPACE, 13),
			scannedValue(protocol.SQLToken_STRING, 21, "it's"), scanned(protocol.SQLToken_WHITESPACE, 22),
			scannedValue(protocol.SQLToken_IDENTIFIER, 26, "FROM"), scanned(protocol.SQLToken_WHITESPACE, 27),
			scannedValue(protocol.SQLToken_IDENTIFIER, 39, "shop/items"), scanned(protocol.SQLToken_SEMICOLON, 40),
		}},
		{"a setting at the head", "--!syntax_v1\n", true, []*protocol.SQLToken{scanned(protocol.SQLToken_UNKNOWN, 12), scanned(protocol.SQLToken_WHITESPACE, 13)}},
		{"the same text inside a query", "--!syntax_v1\n", false, []*protocol.SQLToken{scanned(protocol.SQLToken_COMMENT, 12), scanned(protocol.SQLToken_WHITESPACE, 13)}},
		{"an escape YDB refuses", `'\x1'`, true, []*protocol.SQLToken{scanned(protocol.SQLToken_STRING, 5)}},
		{"an empty text", "", true, make([]*protocol.SQLToken, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := &protocol.ScanSQLRequest{Engine: "ydb", Text: test.text, Head: test.head}
			scan, err := (&backend.Provider{}).ScanSQL(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(scan.Tokens, qt.CmpEquals(protocmp.Transform()), test.want)
			c.Assert(provider.ValidateSQLScan(backend.Description(), request, scan), qt.IsNil)
		})
	}
}
