//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBMigrator_StopsWhenTheLockIsLost: a
// `migrations up` that loses the migration lock stops before its next
// statement and writes nothing more. The migration creates a table, builds an
// index on a filled table, which takes long enough, and creates a second
// table. The lock node is dropped once the revision records the first
// statement, while the build runs: the run reports the
// lock lost, the second table is not created, and the revision keeps what it
// committed before the loss, pending with the first statement applied, rather
// than a failure another runner would have to read past. The host's test lost
// the lock between two statements, which a run of the binary has no pause
// for; the build is the statement in flight here.
func TestHostMigrationsUpStopsWhenTheLockIsLost_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := timeoutFixture(c)
	fillRows(c, f.opened, path.Join(f.database, f.dir, "big"), 1000000)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_x.up.sql": "CREATE TABLE `" + f.table("x") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
			"ALTER TABLE `" + f.table("big") + "` ADD INDEX big_v GLOBAL SYNC ON (v);\n" +
			"CREATE TABLE `" + f.table("y") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_x.down.sql": "DROP TABLE `" + f.table("y") + "`;\nALTER TABLE `" + f.table("big") + "` DROP INDEX big_v;\nDROP TABLE `" + f.table("x") + "`;\n",
	})
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, append([]string{"migrations", "up", "--provider-config", manifest, "--provider-engine", "ydb"}, f.target(migrations)...)...)
	var output lockedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	c.Assert(cmd.Start(), qt.IsNil)

	created := awaitListed(c, f, "x", 2*time.Minute) && awaitFirstStatement(c, f, time.Minute)
	dropped := f.opened.Driver.Coordination().DropNode(ctx, path.Join(f.database, locking.NodeName))
	waitErr := cmd.Wait()

	c.Assert(created, qt.IsTrue, qt.Commentf("%s", output.String()))
	c.Assert(dropped, qt.IsNil)
	c.Assert(waitErr, qt.ErrorMatches, "exit status [1-9][0-9]*", qt.Commentf("%s", output.String()))
	c.Assert(output.String(), qt.Contains, `advisory lock "ptah_migrate" on ydb was lost while it was held`)
	c.Assert(f.names(c), qt.Not(qt.Contains), "y")
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", protocol.HistoryRevision_PENDING, 1, 3}})
}

// awaitFirstStatement waits until the fixture's revision records the
// first statement of its migration applied, for at most limit, and reports
// whether it did. It reads the revision through one history session of its
// own, opened before the wait.
func awaitFirstStatement(c *qt.C, f migrationsFixture, limit time.Duration) bool {
	c.Helper()
	binary, digest := providerBinary(c)
	client, session, closeSession := historySession(c, binary, digest)
	defer closeSession()
	scope := &protocol.HistoryScope{SessionId: session, Schema: f.dir, Table: "schema_migrations", Format: protocol.HistoryScope_NATIVE}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		recorded := slices.ContainsFunc(historyRead(c, client, scope).GetRevisions(), func(revision *protocol.HistoryRevision) bool { return revision.GetApplied() >= 1 })
		if recorded {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// awaitListed waits until the fixture's directory lists name, for at most
// limit, and reports whether it did.
func awaitListed(c *qt.C, f migrationsFixture, name string, limit time.Duration) bool {
	c.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		listed := slices.Contains(f.names(c), name)
		if listed {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
