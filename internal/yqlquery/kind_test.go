package yqlquery_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// ANALYZE, BACKUP and RESTORE each run as a query of their own, outside a
// transaction, as a scheme statement runs. Ported from the host's
// TestKindOf_HappyPath.
func TestSplitRunsMaintenanceStatementsAsSchemeQueries(t *testing.T) {
	for _, text := range []string{"ANALYZE `orders`", "BACKUP `nightly`", "RESTORE `nightly`"} {
		t.Run(text, func(t *testing.T) {
			c := testcheck.New(t)
			queries, err := yqlquery.Split(text)
			c.Assert(err, qt.IsNil)
			c.Assert(queries, qt.HasLen, 1)
			c.Assert(queries[0].Kind, qt.Equals, yqlquery.Scheme)
		})
	}
}
