//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"regexp"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// mirrorIn reports a snapshot that holds the replication mirror in state.
func mirrorIn(state string) func(*protocol.Schema) bool {
	return func(snapshot *protocol.Schema) bool {
		value := object(snapshot, schema.ReplicationProperties, "mirror")
		if value == nil {
			return false
		}
		_, held, err := schema.ObservedReplication(value)
		return err == nil && held == state
	}
}

// Ported from the host's TestYDBReplication_FailedOver: a replication failed
// over by hand keeps its replica as an ordinary table. A schema that keeps the
// replication and leaves the table out is refused, and the table stays: the
// plan would drop a table the replication created and the schema still owns
// through it. The host names the declared replication without its database
// and the read one with the session's, and the refusal finds the declaration
// beside the read replication whatever the database the two name. A schema
// that declares the table and removes the replication drops the replication
// without CASCADE and keeps the table, which takes a write, and plans nothing
// after.
func TestHostDropsAFailedOverReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	f := &replicationFixture{c: c, ctx: c.Context(), opened: opened, client: client, session: session, directory: directory, database: database}
	f.drop("DROP ASYNC REPLICATION `"+f.absolute("mirror")+"` CASCADE", "DROP TABLE `"+f.absolute("rep")+"`", "DROP TABLE `"+f.absolute("src")+"`")
	tables := func(names ...string) string {
		text := "tables:\n"
		for _, name := range names {
			text += "  " + name + ":\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"
		}
		return text
	}
	mirror := "async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + f.absolute("src") + "\n        target: " + path.Join(directory, "rep") + "\n"
	hostRun(c, directory, "the table", tables("src"), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "the replication", tables("src")+mirror, `(?s).*Schema apply completed successfully.*`)
	f.await("the replica written", func(*protocol.Schema) bool { return f.replicaWritten("rep") })
	f.exec("ALTER ASYNC REPLICATION `" + f.absolute("mirror") + "` SET (STATE = 'DONE', FAILOVER_MODE = 'FORCE')")
	f.await("the replication failed over", mirrorIn(replication.StateDone))

	kept, keptErr := hostApplySource(c, directory, "yaml", tables("src")+mirror)
	removed, removedErr := hostApplySource(c, directory, "yaml", tables("src", "rep")+"async_replications: {}\n")

	c.Assert(keptErr, qt.IsNotNil, qt.Commentf("%s", kept))
	c.Assert(kept, qt.Matches, `(?s).*table `+regexp.QuoteMeta(directory)+`/rep: it is a table async replication `+regexp.QuoteMeta(directory)+
		`/mirror created and failed over, which the schema keeps; .*`)
	c.Assert(removedErr, qt.IsNil, qt.Commentf("%s", removed))
	c.Assert(removed, qt.Contains, "DROP ASYNC REPLICATION without CASCADE ends the replication and keeps its tables")
	c.Assert(removed, qt.Not(qt.Contains), "CASCADE drops the replica tables")
	c.Assert(object(f.inspect(), schema.ReplicationProperties, "mirror"), qt.IsNil)
	f.exec("UPSERT INTO `" + f.absolute("rep") + "` (id, note) VALUES (1, 'written'u)")
	assertRowCount(c, opened, f.absolute("rep"), 1)
	hostRun(c, directory, "a run after the removal", tables("src", "rep")+"async_replications: {}\n", `(?s).*Schema is synced, no changes to be made.*`)
}
