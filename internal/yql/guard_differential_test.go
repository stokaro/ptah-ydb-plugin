package yql_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// scopes.json holds the host's YDB replay guard's verdict on each corpus
// statement under every replay scope, recorded by testdata/realm/oracle. The
// host's guard reads a run that owns the whole server, or a server the run
// provisioned, as the server; every other scope confines a statement to the
// dev database.
type scopedRow struct {
	Statement string `json:"statement"`
	Scopes    map[string]struct {
		Outcome   string `json:"outcome"`
		Operation string `json:"operation"`
	} `json:"scopes"`
}

func TestGuardReplayAgreesWithHost(t *testing.T) {
	c := qt.New(t)
	raw, err := os.ReadFile("testdata/realm/scopes.json")
	c.Assert(err, qt.IsNil)
	var golden struct {
		Rows []scopedRow `json:"rows"`
	}
	c.Assert(json.Unmarshal(raw, &golden), qt.IsNil)
	c.Assert(len(golden.Rows) >= 90, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(golden.Rows)))
	server := map[string]bool{"database": false, "session": false, "databases": false, "server": true, "provisioned": true}
	for _, row := range golden.Rows {
		c.Assert(row.Scopes, qt.HasLen, len(server))
		for scope, verdict := range row.Scopes {
			t.Run(scope+"/"+row.Statement, func(t *testing.T) {
				c := qt.New(t)

				err := yql.GuardReplay(row.Statement, server[scope])

				var refusal *yql.RealmRefusal
				c.Assert(errors.As(err, &refusal), qt.Equals, verdict.Outcome == "refused")
				c.Assert(refusal.OperationOrEmpty(), qt.Equals, verdict.Operation)
			})
		}
	}
}
