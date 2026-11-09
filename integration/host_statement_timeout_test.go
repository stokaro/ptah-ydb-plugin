//go:build integration

package integration_test

import (
	"fmt"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests in this file run `migrations up --statement-timeout` through this
// provider. The host passes the timeout with each unit, and the provider says
// what became of a unit the timeout stopped.

// fillRows creates the table at absolute with rows rows of about 80 bytes
// each, generated on the server a batch per query, so a statement over it
// outlives a short statement timeout.
func fillRows(c *qt.C, opened *connection.Connection, absolute string, rows int) {
	c.Helper()
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, v Utf8, w Utf8, PRIMARY KEY (id))"), qt.IsNil)
	const batch = 20000
	for start := 0; start < rows; start += batch {
		_, err := opened.DB.ExecContext(c.Context(), fmt.Sprintf("UPSERT INTO `%s` (id, v, w) SELECT CAST(x AS Int64) AS id, "+
			"CAST(x AS Utf8) || 'value-padding-padding-padding'u AS v, 'more-padding-more-padding-more-padding'u AS w "+
			"FROM AS_TABLE(ListMap(ListFromRange(%d, %d), ($x) -> (AsStruct($x AS x))))", absolute, start, min(start+batch, rows)))
		c.Assert(err, qt.IsNil)
	}
}

// timeoutFixture is a migrations fixture whose directory exists before the
// first run, so a test can create the tables its migration reads.
func timeoutFixture(c *qt.C) migrationsFixture {
	c.Helper()
	f := newMigrationsFixture(c)
	c.Assert(f.opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(f.database, f.dir)), qt.IsNil)
	return f
}

// Ported from the host's TestYDBMigrator_StatementTimeoutCancelsAnIndexBuild:
// an index build outlives the statement timeout, and YDB keeps building after
// the client stops waiting. The provider cancels the build, so the run fails
// saying nothing was applied, the revision records the query as not run, and
// the table holds no index. The same file run again without the timeout
// resumes at the query and builds the index. The build is found by the table
// the statement names, read the way the server reads it: a quoted name may
// carry a C escape, so `big\x2Descaped` alters the table big-escaped.
func TestHostStatementTimeoutCancelsAnIndexBuild_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, table, spelled string
	}{
		{name: "a plain name", table: "big", spelled: "big"},
		{name: "a name written with an escape", table: "big-escaped", spelled: "big\\x2Descaped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			f := timeoutFixture(c)
			absolute := path.Join(f.database, f.dir, test.table)
			fillRows(c, f.opened, absolute, 1000000)
			migrations := writeFiles(c, c.TempDir(), map[string]string{
				"0000000001_index.up.sql":   "ALTER TABLE `" + f.dir + "/" + test.spelled + "` ADD INDEX big_v GLOBAL SYNC ON (v);\n",
				"0000000001_index.down.sql": "ALTER TABLE `" + f.dir + "/" + test.spelled + "` DROP INDEX big_v;\n",
			})

			failure, failErr := hostCommand(c, append([]string{"migrations", "up", "--statement-timeout", "300ms"}, f.target(migrations)...)...)
			stopped := f.progress(c, protocol.HistoryScope_NATIVE)
			indexes := heldIndexNames(c, f.opened, absolute)
			resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(migrations)...)...)

			c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))
			c.Assert(failure, qt.Contains, "failed to apply migration 1: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
				"provider execution: NOT_APPLIED at the statement timeout (0 units confirmed applied): the statement timeout stopped the unit, and nothing of it was applied")
			c.Assert(stopped, qt.DeepEquals, []progress{{"1", failed, 0, 1}})
			c.Assert(indexes, qt.HasLen, 0)
			c.Assert(resumeErr, qt.IsNil, qt.Commentf("%s", resumed))
			c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 1, 1}})
			c.Assert(heldIndexNames(c, f.opened, absolute), qt.DeepEquals, []string{"big_v"})
		})
	}
}

// Ported from the host's TestYDBMigrator_StatementTimeoutCancelsADataQuery: a
// data query outlives the statement timeout. YDB cancels it, and its
// transaction, which holds the checkpoint too, never commits: the target stays
// empty and the revision records the query as not run. The same file run
// again without the timeout copies every row once.
func TestHostStatementTimeoutCancelsADataQuery_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := timeoutFixture(c)
	fillRows(c, f.opened, path.Join(f.database, f.dir, "src"), 300000)
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "CREATE TABLE `"+path.Join(f.database, f.dir, "dst")+"` (id Int64 NOT NULL, v Utf8, w Utf8, PRIMARY KEY (id))"), qt.IsNil)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_copy.up.sql":   "INSERT INTO `" + f.table("dst") + "` (id, v, w) SELECT id, v, w FROM `" + f.table("src") + "`;\n",
		"0000000001_copy.down.sql": "DELETE FROM `" + f.table("dst") + "`;\n",
	})

	failure, failErr := hostCommand(c, append([]string{"migrations", "up", "--statement-timeout", "200ms"}, f.target(migrations)...)...)
	stopped := f.progress(c, protocol.HistoryScope_NATIVE)
	copied := f.count(c, "SELECT COUNT(*) FROM `"+f.table("dst")+"`")
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(migrations)...)...)

	c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))
	c.Assert(failure, qt.Contains, "failed to apply migration 1: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
		"provider execution: NOT_APPLIED at the statement timeout (0 units confirmed applied): the statement timeout stopped the unit, and nothing of it was applied")
	c.Assert(stopped, qt.DeepEquals, []progress{{"1", failed, 0, 1}})
	c.Assert(copied, qt.Equals, int64(0))
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("%s", resumed))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("dst")+"`"), qt.Equals, int64(300000))
}
