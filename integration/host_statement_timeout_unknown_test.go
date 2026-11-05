//go:build integration

package integration_test

import (
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBMigrator_StatementTimeoutLeavesAQuickSchemaChangeUnknown:
// a schema statement YDB runs as a quick change rather than a build cannot be
// canceled, and keeps running when the client stops waiting. `migrations up
// --statement-timeout 1ms` stops waiting for one, so whether it was applied is
// unknown: the run says so, and a resume refuses to guess. The provider checks
// the session's lock before the statement under the run's own context, so the
// timeout, which ends long before that check does, stops the statement and
// does not take the lock away.
func TestHostStatementTimeoutLeavesAQuickSchemaChangeUnknown_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	c.Assert(f.opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(f.database, f.dir)), qt.IsNil)
	absolute := path.Join(f.database, f.dir, "t")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_column.up.sql":   "ALTER TABLE `" + f.table("t") + "` ADD COLUMN note Utf8;\n",
		"0000000001_column.down.sql": "ALTER TABLE `" + f.table("t") + "` DROP COLUMN note;\n",
	})

	failure, failErr := hostCommand(c, append([]string{"migrations", "up", "--statement-timeout", "1ms"}, f.target(migrations)...)...)
	settleSchemaChange(c, f.opened, absolute, "note")
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(migrations)...)...)

	c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))
	c.Assert(failure, qt.Contains, "failed to apply migration 1: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
		"operation outcome is unknown: provider execution: UNKNOWN at the statement timeout (0 units confirmed applied): "+
		"the statement timeout stopped waiting for the unit, which YDB may still apply")
	c.Assert(failure, qt.Not(qt.Contains), "was lost while it was held")
	c.Assert(resumeErr, qt.IsNotNil, qt.Commentf("%s", resumed))
	c.Assert(resumed, qt.Contains, "migration 1 cannot resume automatically: the outcome of statement 1 is unknown")
}

// settleSchemaChange waits until column exists in the table at absolute, or
// until a schema change sent before its client gave up has had time to land,
// so the test's cleanup does not race it.
func settleSchemaChange(c *qt.C, opened *connection.Connection, absolute, column string) {
	c.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, held := heldColumnTypes(c, opened, absolute)[column]; held {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
