package yql_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// The corpus holds the statements the host's YDB replay guard (internal/devclean/ydbreplay)
// was run over, with the dev database scope that confines a statement to its
// directory, and its answers. testdata/realm/README.md says how host.json is
// regenerated.
type guardRow struct {
	Statement string `json:"statement"`
	Outcome   string `json:"outcome"`
	Operation string `json:"operation"`
}

func readGuardCorpus(c *qt.C) []guardRow {
	raw, err := os.ReadFile("testdata/realm/host.json")
	c.Assert(err, qt.IsNil)
	var rows []guardRow
	c.Assert(json.Unmarshal(raw, &rows), qt.IsNil)
	return rows
}

func TestConfineToRealmAgreesWithHost(t *testing.T) {
	c := qt.New(t)
	rows := readGuardCorpus(c)
	c.Assert(len(rows) >= 80, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(rows)))
	for _, row := range rows {
		t.Run(row.Statement, func(t *testing.T) {
			c := qt.New(t)

			err := yql.ConfineToRealm(row.Statement)

			var refusal *yql.RealmRefusal
			c.Assert(errors.As(err, &refusal), qt.Equals, row.Outcome == "refused")
			c.Assert(refusal.OperationOrEmpty(), qt.Equals, row.Operation)
		})
	}
}
