//go:build integration

package integration_test

import (
	"fmt"
	"path"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replicationOf is the replication name the fixture's directory holds, read
// through the provider, or a zero spec and false where it holds none.
func replicationOf(f *replicationFixture, name string) (replication.ReplicationSpec, bool) {
	f.c.Helper()
	value := object(f.inspect(), schema.ReplicationProperties, name)
	if value == nil {
		return replication.ReplicationSpec{}, false
	}
	spec, _, err := schema.ObservedReplication(value)
	f.c.Assert(err, qt.IsNil)
	return spec, true
}

// Ported from the replication rows of the host's
// TestPlanBidirectionalSchemaDiff_ReplicationsRollBack (migration/generator): a
// migration that creates an async replication, drops a running one and moves a
// paused one's connection rolls back by dropping the created one with CASCADE,
// creating the dropped one again from what the read found, and moving the
// paused one's connection back, each with what it cannot recover. Both files
// apply. The built-in engine at 74de0b65b refused this rollback on 25.1.4.7
// and 26.2.1.14 (`its target ... holds ... a replica table of another
// replication`): it took the replica the forward migration drops with its
// replication for one that stays.
func TestHostRollsBackReplicationChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := yqlReplicationFixture(c)
	emptyAtCleanup(c, f.opened, path.Join(f.database, f.directory))
	f.drop("DROP ASYNC REPLICATION `"+f.absolute("mirror")+"` CASCADE", "DROP ASYNC REPLICATION `"+f.absolute("old")+"` CASCADE",
		"DROP ASYNC REPLICATION `"+f.absolute("paused")+"` CASCADE")
	connection := selfConnection(c, f.opened)
	moved := alternateSelfConnection(c, connection)
	tables := "tables:\n"
	for _, name := range []string{"s1", "s2", "s3"} {
		f.exec("CREATE TABLE `" + f.absolute(name) + "` (id Int64 NOT NULL, PRIMARY KEY (id))")
		tables += "  " + name + ":\n    schema: " + f.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"
	}
	held := func(name, source, target string) {
		f.exec(fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s')", f.absolute(name), f.absolute(source), f.absolute(target), connection))
	}
	held("old", "s1", "r1")
	held("paused", "s2", "r2")
	f.await("the replicas written", func(*protocol.Schema) bool { return f.replicaWritten("r1") && f.replicaWritten("r2") })
	f.exec("ALTER ASYNC REPLICATION `" + f.absolute("paused") + "` SET (STATE = 'PAUSED')")
	f.await("the replication paused", func(snapshot *protocol.Schema) bool {
		_, state, err := schema.ObservedReplication(object(snapshot, schema.ReplicationProperties, "paused"))
		return err == nil && state == replication.StatePaused
	})
	item := func(source, target string) string {
		return "\n    items:\n      - {source: " + f.absolute(source) + ", target: " + path.Join(f.directory, target) + "}\n"
	}
	declared := tables + "async_replications:\n" +
		"  mirror:\n    schema: " + f.directory + "\n    connection_string: " + strconv.Quote(connection) + item("s3", "ra") +
		"  paused:\n    schema: " + f.directory + "\n    connection_string: " + strconv.Quote(moved) + item("s2", "r2")

	migrations, up, down := hostGenerate(c, f.directory, "yaml", declared)
	// The host rates DROP ASYNC REPLICATION ... CASCADE destructive (DS107).
	upped, upErr := hostMigrate(c, f.directory, migrations, "migrations", "up", "--allow-destructive")
	_, oldAfterUp := replicationOf(f, "old")
	downed, downErr := hostMigrate(c, f.directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(down, qt.Contains, "DROP ASYNC REPLICATION `"+path.Join(f.directory, "mirror")+"` CASCADE;")
	c.Assert(down, qt.Contains, fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s');",
		path.Join(f.directory, "old"), path.Join(f.directory, "s1"), path.Join(f.directory, "r1"), connection))
	c.Assert(down, qt.Contains, fmt.Sprintf("ALTER ASYNC REPLICATION `%s` SET (CONNECTION_STRING = '%s');", path.Join(f.directory, "paused"), connection))
	c.Assert(down, qt.Contains, fmt.Sprintf(`-- Recovery limit: "dropping async replication %s drops the replica tables it created, with every row it copied"`, path.Join(f.directory, "mirror")))
	c.Assert(down, qt.Contains, fmt.Sprintf(`-- Recovery limit: "the replica tables async replication %s dropped are created again empty, and the replication copies its source again from the start"`,
		path.Join(f.directory, "old")))
	c.Assert(up, qt.Contains, "DROP ASYNC REPLICATION `"+path.Join(f.directory, "old")+"` CASCADE;")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", upped))
	c.Assert(oldAfterUp, qt.IsFalse)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", downed, down))
	_, mirrorAfterDown := replicationOf(f, "mirror")
	c.Assert(mirrorAfterDown, qt.IsFalse)
	_, oldAfterDown := replicationOf(f, "old")
	c.Assert(oldAfterDown, qt.IsTrue)
	paused, _ := replicationOf(f, "paused")
	c.Assert(paused.Connection.ConnectionString, qt.Equals, connection)
}

// Ported from the transfer row of the host's
// TestPlanBidirectionalSchemaDiff_ReplicationsRollBack: a migration that
// changes a transfer's lambda rolls back by setting the lambda the transfer
// had, in place, and says the rows written meanwhile stay. Both files apply.
// The built-in engine at 74de0b65b and this provider refused the rollback on
// 26.2.1.14 (`its consumer differ from the database's`), comparing the
// consumer YDB made for the transfer with the none the declaration names
// (G233).
func TestHostRollsBackATransferLambda_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := yqlReplicationFixture(c)
	skipWithout(c, f.session, replication.CapabilityTransfers)
	emptyAtCleanup(c, f.opened, path.Join(f.database, f.directory))
	f.drop("DROP TRANSFER `"+f.absolute("ingest")+"`", "DROP TOPIC `"+f.absolute("events")+"`")
	const held, changed = "($msg) -> { RETURN [<|id:Unwrap(CAST($msg._offset AS Int64))|>]; }", "($msg) -> { RETURN [<|id:Unwrap(CAST($msg._offset AS Int64)) + 1|>]; }"
	f.exec("CREATE TOPIC `" + f.absolute("events") + "`")
	f.exec("CREATE TABLE `" + f.absolute("rows") + "` (id Int64 NOT NULL, PRIMARY KEY (id))")
	f.exec("CREATE TRANSFER `" + f.absolute("ingest") + "` FROM `" + path.Join(f.directory, "events") + "` TO `" + f.absolute("rows") + "` USING " + held)
	declared := "tables:\n  rows:\n    schema: " + f.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"topics:\n  events:\n    schema: " + f.directory + "\n" +
		"transfers:\n  ingest:\n    schema: " + f.directory + "\n    source: " + path.Join(f.directory, "events") + "\n    target: " + path.Join(f.directory, "rows") +
		"\n    using: " + strconv.Quote(changed) + "\n"
	lambda := func() string {
		c.Helper()
		value := object(f.inspect(), schema.TransferProperties, "ingest")
		c.Assert(value, qt.IsNotNil)
		spec, _, err := schema.ObservedTransfer(value)
		c.Assert(err, qt.IsNil)
		return spec.Lambda
	}

	migrations, up, down := hostGenerate(c, f.directory, "yaml", declared)
	upped, upErr := hostMigrate(c, f.directory, migrations, "migrations", "up")
	afterUp := lambda()
	downed, downErr := hostMigrate(c, f.directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER TRANSFER `"+path.Join(f.directory, "ingest")+"` SET USING "+changed+";")
	c.Assert(down, qt.Contains, "ALTER TRANSFER `"+path.Join(f.directory, "ingest")+"` SET USING "+held+";")
	c.Assert(down, qt.Contains, fmt.Sprintf(`-- Recovery limit: "the rows transfer %s wrote with the changed lambda stay as written"`, path.Join(f.directory, "ingest")))
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", upped))
	c.Assert(afterUp, qt.Equals, changed)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", downed))
	c.Assert(lambda(), qt.Equals, held)
}
