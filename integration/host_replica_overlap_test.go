//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's TestGenerateMigrationAST_Replications_FailurePath,
// "a replication created over another's replica"
// (internal/planner/dialects/ydb): a schema that adds a replication whose
// target is the replica another replication writes, a table or a directory,
// is refused before anything runs, and the other replication and its replica
// stay. YDB accepts such a replication, measured on 25.1.4.7 and 26.2.1.14:
// both then write the replica, and a later drop of the new one with CASCADE
// drops the replica while the other replication runs on without it. This
// provider planned and applied it (G242). The host's embedded engine at
// 74de0b65b refused the plan, in other words: live, its planner refused two
// unordered steps on the replica's path.
func TestHostRefusesAReplicationOverAnothersReplica_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name                     string
		source, source2, replica string
		// written is the replica table first writes, as YDB describes the
		// replication's item: a directory's tables one by one.
		written string
		// tables declares the sources, with {dir} for the fixture directory.
		tables string
	}{
		{name: "a replica table", source: "src", source2: "src2", replica: "rep", written: "rep",
			tables: "  src:\n    schema: {dir}\n" + overlapColumns + "  src2:\n    schema: {dir}\n" + overlapColumns},
		{name: "a replica directory", source: "sdir", source2: "sdir2", replica: "repdir", written: "repdir/accounts",
			tables: "  accounts:\n    schema: {dir}/sdir\n" + overlapColumns + "  accounts2:\n    name: accounts\n    schema: {dir}/sdir2\n" + overlapColumns},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			client := startProviderFor(c)
			session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
			f := &replicationFixture{c: c, ctx: c.Context(), opened: opened, client: client, session: session, directory: directory, database: database}
			// The replications go first, with their replicas, and the tree
			// after them.
			emptyAtCleanup(c, opened, path.Join(database, directory))
			f.drop("DROP ASYNC REPLICATION `"+f.absolute("second")+"` CASCADE", "DROP ASYNC REPLICATION `"+f.absolute("first")+"` CASCADE")
			declared := strings.ReplaceAll("tables:\n"+test.tables, "{dir}", directory)
			for statement := range strings.SplitSeq(strings.TrimSpace(strings.ReplaceAll(overlapSources[test.name], "{root}", path.Join(database, directory))), "\n") {
				f.exec(statement)
			}
			connection := selfConnection(c, opened)
			f.exec("CREATE ASYNC REPLICATION `" + f.absolute("first") + "` FOR `" + f.absolute(test.source) + "` AS `" + f.absolute(test.replica) +
				"` WITH (CONNECTION_STRING = '" + connection + "')")
			f.await("the replica", func(*protocol.Schema) bool { return f.replicaWritten(test.replica) || f.replicaDirectory(test.replica) })
			replication := func(name, source string) string {
				return "  " + name + ":\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(connection) +
					"\n    items:\n      - source: " + f.absolute(source) + "\n        target: " + path.Join(directory, test.replica) + "\n"
			}
			kept := declared + "async_replications:\n" + replication("first", test.source)

			synced, syncErr := hostApplySource(c, directory, "yaml", kept)
			refused, refusedErr := hostApplySource(c, directory, "yaml", kept+replication("second", test.source2))

			c.Assert(syncErr, qt.IsNil, qt.Commentf("%s", synced))
			c.Assert(synced, qt.Contains, "Schema is synced, no changes to be made")
			c.Assert(refusedErr, qt.IsNotNil, qt.Commentf("%s", refused))
			c.Assert(refused, qt.Contains, "async replication "+path.Join(directory, "second")+": its target "+path.Join(directory, test.replica)+
				" overlaps "+path.Join(directory, test.written)+", where async replication "+path.Join(directory, "first")+" writes its replica; "+
				"YDB creates the replication over that replica, both then write it, and a drop of either with CASCADE drops it "+
				"while the other runs on without it (measured on 25.1.4.7 and 26.2.1.14)")
			snapshot := f.inspect()
			c.Assert(object(snapshot, schema.ReplicationProperties, "second"), qt.IsNil)
			c.Assert(object(snapshot, schema.ReplicationProperties, "first"), qt.IsNotNil)
			c.Assert(f.replicaWritten(test.replica) || f.replicaDirectory(test.replica), qt.IsTrue)
		})
	}
}

// overlapColumns are the columns of every table the overlap test declares.
const overlapColumns = "    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"

// overlapSources creates the sources of each row of the overlap test, one
// statement to a line, with {root} for the fixture directory's absolute path.
var overlapSources = map[string]string{
	"a replica table": "CREATE TABLE `{root}/src` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))\n" +
		"CREATE TABLE `{root}/src2` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))",
	"a replica directory": "CREATE TABLE `{root}/sdir/accounts` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))\n" +
		"CREATE TABLE `{root}/sdir2/accounts` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))",
}

// replicaDirectory reports whether the directory name of the fixture holds
// the replica table accounts.
func (f *replicationFixture) replicaDirectory(name string) bool {
	f.c.Helper()
	listing, err := f.opened.Driver.Scheme().ListDirectory(f.ctx, path.Join(f.database, f.directory, name))
	return err == nil && len(listing.Children) == 1 && listing.Children[0].Name == "accounts" && listing.Children[0].IsTable()
}
