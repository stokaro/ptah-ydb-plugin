package migrationlint_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A unique index added to an existing table names the capability it needs,
// so a cluster whose capabilities allow it reports nothing, and one that
// lacks it reports the requirement. Ported from the host's
// TestLintSource_YDB_LeavesWhatTheTargetAccepts.
func TestUniqueIndexOnAnExistingTableFollowsTheCapability(t *testing.T) {
	const sql = "ALTER TABLE t ADD INDEX t_v GLOBAL UNIQUE ON (v);"
	for _, test := range []struct {
		name string
		held bool
		want []string
	}{
		{name: "a cluster that allows it", held: true, want: make([]string, 0)},
		{name: "a cluster that does not", want: []string{"1:1 CAP001 unique_index_on_existing_table"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := ownedFindings(c, sql, map[string]bool{"unique_index_on_existing_table": test.held})
			c.Assert(append(make([]string, 0), got...), qt.DeepEquals, test.want)
		})
	}
}
