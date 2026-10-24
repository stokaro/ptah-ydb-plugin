package connection_test

import (
	"cmp"
	"encoding/json"
	"os"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// The corpus holds the addresses the host's YDB connection layer was run over
// and its answers. A row is the host's verdict on the whole address: the URL
// reader (internal/ydburl), the SDK parameter checks and the credential
// choice (internal/dbschema/ydb). testdata/oracle/README.md says how the file
// is regenerated. Every row must get the host's verdict and, when the host
// accepts it, the SDK address the host hands ydb-go-sdk, unless divergences
// names the row and says why.
type hostRow struct {
	URL      string `json:"url"`
	Outcome  string `json:"outcome"`
	Error    string `json:"error"`
	Database string `json:"database"`
	DSN      string `json:"dsn"`
}

// divergence is a row on which this package answers differently from the host
// on purpose.
type divergence struct {
	Outcome string
	Reason  string
}

// divergences are the rows this package refuses and the host accepts.
var divergences = map[string]divergence{
	"ydb://host/local#frag": {"refused",
		"a fragment is never part of an address, and accepting one hides a mistyped URL"},
	"ydb://host/local?token=a%zz": {"refused",
		"the host reads a query it cannot unescape as empty, so a token with a bad escape connects anonymously"},
}

func readCorpus(c *qt.C) []hostRow {
	raw, err := os.ReadFile("testdata/oracle/host.json")
	c.Assert(err, qt.IsNil)
	var rows []hostRow
	c.Assert(json.Unmarshal(raw, &rows), qt.IsNil)
	return rows
}

// divergenceOf returns the divergence recorded for row, or the zero value.
func divergenceOf(row hostRow) divergence {
	if strings.Contains(row.URL, "dev_realm") {
		return divergence{"refused", "the address does not read a dev realm"}
	}
	return divergences[row.URL]
}

// agreed returns the rows both the host and this package accept.
func agreed(rows []hostRow) []hostRow {
	var kept []hostRow
	for _, row := range rows {
		if row.Outcome == "ok" && divergenceOf(row).Outcome == "" {
			kept = append(kept, row)
		}
	}
	return kept
}

func TestAddressAgreesWithHost_Verdict(t *testing.T) {
	c := qt.New(t)
	rows := readCorpus(c)
	c.Assert(len(rows) >= 80, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(rows)))
	for _, row := range rows {
		t.Run(row.URL, func(t *testing.T) {
			c := qt.New(t)
			c.Setenv("YDB_ACCESS_TOKEN_CREDENTIALS", "test-secret")

			_, err := connection.Parse(row.URL)

			verdict := map[bool]string{true: "refused", false: "ok"}[err != nil]
			c.Assert(verdict, qt.Equals, cmp.Or(divergenceOf(row).Outcome, row.Outcome))
		})
	}
}

func TestAddressAgreesWithHost_SDKAddress(t *testing.T) {
	c := qt.New(t)
	rows := agreed(readCorpus(c))
	c.Assert(len(rows) >= 25, qt.IsTrue, qt.Commentf("the corpus holds %d accepted rows", len(rows)))
	for _, row := range rows {
		t.Run(row.URL, func(t *testing.T) {
			c := qt.New(t)

			address, err := connection.Parse(row.URL)

			c.Assert(err, qt.IsNil)
			c.Assert(address.String(), qt.Equals, row.DSN)
			c.Assert(address.Database(), qt.Equals, row.Database)
		})
	}
}
