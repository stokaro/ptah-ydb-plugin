//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// yqlReplicationFixture is a lock fixture for the YQL replication and transfer
// tests, read back through a provider session.
func yqlReplicationFixture(c *qt.C) *replicationFixture {
	c.Helper()
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &replicationFixture{c: c, ctx: c.Context(), opened: opened, client: client, session: session, directory: directory, database: database}
}

// yqlApply runs the host's schema apply over a YQL schema and requires want
// of its answer.
func yqlApply(f *replicationFixture, name, text, want string) string {
	f.c.Helper()
	output, err := hostApplySource(f.c, f.directory, "sql", text)
	f.c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	f.c.Assert(output, qt.Contains, want, qt.Commentf("%s: %s", name, output))
	return output
}

// heldReplication is the replication mirror the fixture's directory holds,
// and its state, read through the provider.
func heldReplication(f *replicationFixture) (replication.ReplicationSpec, string) {
	f.c.Helper()
	value := object(f.inspect(), schema.ReplicationProperties, "mirror")
	f.c.Assert(value, qt.IsNotNil)
	spec, state, err := schema.ObservedReplication(value)
	f.c.Assert(err, qt.IsNil)
	return spec, state
}

// Ported from the host's TestYDBDesiredYQL_Replication: a YQL schema declares
// an async replication of the server's own table with CREATE ASYNC
// REPLICATION, which applies and plans nothing once the replica is written.
// The same schema with an ALTER ASYNC REPLICATION ... SET (CONNECTION_STRING)
// is refused while the replication runs, since YDB changes a connection only
// while it is paused, and applies once it is paused. A schema that leaves the
// replication out drops it with its replica. The source table is applied
// first, as TestHostAppliesAnAsyncReplication_HappyPath applies it.
func TestHostAppliesAYQLReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := yqlReplicationFixture(c)
	f.drop("DROP ASYNC REPLICATION `"+f.absolute("mirror")+"` CASCADE", "DROP TABLE `"+f.absolute("rep")+"`", "DROP TABLE `"+f.absolute("src")+"`")
	connection := selfConnection(c, f.opened)
	moved := alternateSelfConnection(c, connection)
	table := "CREATE TABLE `" + path.Join(f.directory, "src") + "` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id));\n"
	declared := table + fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s');\n",
		path.Join(f.directory, "mirror"), f.absolute("src"), path.Join(f.directory, "rep"), connection)
	changed := declared + fmt.Sprintf("ALTER ASYNC REPLICATION `%s` SET (CONNECTION_STRING = '%s');\n", path.Join(f.directory, "mirror"), moved)
	yqlApply(f, "the table", table, "Schema apply completed successfully")

	yqlApply(f, "the replication", declared, "Schema apply completed successfully")
	f.await("the replica written", func(*protocol.Schema) bool { return f.replicaWritten("rep") })
	yqlApply(f, "a second run", declared, "Schema is synced, no changes to be made")
	running, runningErr := hostApplySource(c, f.directory, "sql", changed)
	f.exec("ALTER ASYNC REPLICATION `" + f.absolute("mirror") + "` SET (STATE = 'PAUSED')")
	f.await("the replication paused", func(*protocol.Schema) bool { _, state := heldReplication(f); return state == replication.StatePaused })
	yqlApply(f, "the change while paused", changed, "Schema apply completed successfully")

	c.Assert(runningErr, qt.IsNotNil, qt.Commentf("%s", running))
	c.Assert(running, qt.Matches, `(?s).*its connection or credential differs, and YDB changes them only while the replication is paused .*it is running.*`)
	spec, state := heldReplication(f)
	c.Assert(state, qt.Equals, replication.StatePaused)
	c.Assert(spec.Connection.ConnectionString, qt.Equals, moved)
	yqlApply(f, "a run after the change", changed, "Schema is synced, no changes to be made")
	removed := yqlApply(f, "the removal", table, "Schema apply completed successfully")
	c.Assert(removed, qt.Contains, "DROP ASYNC REPLICATION ... CASCADE drops the replica tables with the replication")
	awaitNames(c, f.opened, path.Join(f.database, f.directory), []string{"src"})
	yqlApply(f, "a run after the removal", table, "Schema is synced, no changes to be made")
}

// yqlTransferObjects declares, in YQL, the topic events and the table rows a
// transfer of the fixture's directory reads and writes.
func yqlTransferObjects(f *replicationFixture) string {
	return "CREATE TOPIC `" + path.Join(f.directory, "events") + "`;\nCREATE TABLE `" + path.Join(f.directory, "rows") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n"
}

// yqlTransfer declares, in YQL, the transfer ingest from events into rows.
func yqlTransfer(f *replicationFixture) string {
	return yqlTransferObjects(f) + "CREATE TRANSFER `" + path.Join(f.directory, "ingest") + "` FROM `" + path.Join(f.directory, "events") + "` TO `" + path.Join(f.directory, "rows") +
		"` USING ($msg) -> { RETURN [<|id:Unwrap(CAST($msg._offset AS Int64))|>]; } WITH (FLUSH_INTERVAL = Interval('PT1S'));\n"
}

// Ported from the host's TestYDBDesiredYQL_Transfer: a YQL schema declares a
// transfer from a topic into a table with CREATE TRANSFER, which applies and
// plans nothing once it runs: the consumer YDB made for it stays on the topic
// the schema declares without consumers. An ALTER TRANSFER ... SET USING
// ..., SET (BATCH_SIZE_BYTES, FLUSH_INTERVAL) is one change in place, which
// reads back. A schema that leaves the transfer out drops it, and only it.
func TestHostAppliesAYQLTransfer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := yqlReplicationFixture(c)
	skipWithout(c, f.session, replication.CapabilityTransfers)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TRANSFER `"+f.absolute("ingest")+"`")
		for cleanup.Err() == nil {
			description, err := f.opened.Driver.Topic().Describe(cleanup, f.absolute("events"))
			if err != nil || len(description.Consumers) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TOPIC `"+f.absolute("events")+"`")
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TABLE `"+f.absolute("rows")+"`")
	})
	const lambda = "($msg) -> { RETURN [<|id:Unwrap(CAST($msg._offset AS Int64))+1l|>]; }"
	changed := yqlTransfer(f) + "ALTER TRANSFER `" + path.Join(f.directory, "ingest") + "` SET USING " + lambda + ", SET (BATCH_SIZE_BYTES = 4096, FLUSH_INTERVAL = Interval('PT2S'));\n"
	running := func(*protocol.Schema) bool {
		value := object(f.inspect(), schema.TransferProperties, "ingest")
		if value == nil {
			return false
		}
		spec, state, err := schema.ObservedTransfer(value)
		return err == nil && state == replication.StateRunning && spec.Consumer != ""
	}

	yqlApply(f, "the transfer", yqlTransfer(f), "Schema apply completed successfully")
	f.await("the transfer running through a consumer", running)
	settled := yqlApply(f, "a second run", yqlTransfer(f), "Schema is synced, no changes to be made")
	altered := yqlApply(f, "the change", changed, "Schema apply completed successfully")

	c.Assert(settled, qt.Not(qt.Contains), unitPrefix)
	c.Assert(operationKinds(altered), qt.DeepEquals, []string{"transfer-operation"}, qt.Commentf("%s", altered))
	spec, _, err := schema.ObservedTransfer(object(f.inspect(), schema.TransferProperties, "ingest"))
	c.Assert(err, qt.IsNil)
	c.Assert(spec.Lambda, qt.Equals, lambda)
	c.Assert(spec.BatchSizeBytes, qt.Equals, uint64(4096))
	c.Assert(spec.FlushInterval, qt.Equals, "PT2S")
	yqlApply(f, "a run after the change", changed, "Schema is synced, no changes to be made")
	removed := yqlApply(f, "the removal", yqlTransferObjects(f), "Schema apply completed successfully")
	c.Assert(operationKinds(removed), qt.DeepEquals, []string{"transfer-operation"}, qt.Commentf("%s", removed))
	yqlApply(f, "a run after the removal", yqlTransferObjects(f), "Schema is synced, no changes to be made")
}

// Ported from the host's TestYDBDesiredYQL_TransferRefusedOn251: a YQL
// transfer is refused on a line without transfers, such as 25.1 with its
// default flags, before anything is created. The host reports the provider's
// refusal by its status code.
func TestHostRefusesAYQLTransferWithoutTheCapability_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := yqlReplicationFixture(c)
	requireNoTransfers(c, f)

	output, err := hostApplySource(c, f.directory, "sql", yqlTransfer(f))

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "provider compare models failed (InvalidArgument)")
	c.Assert(listedNames(c, f.opened, path.Join(f.database, f.directory)), qt.HasLen, 0)
}

// requireNoTransfers skips a test on a target with transfers.
func requireNoTransfers(c *qt.C, f *replicationFixture) {
	c.Helper()
	if f.session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target supports transfers")
	}
}

// operationKinds lists the kinds of the native operations an apply planned,
// in plan order.
func operationKinds(output string) []string {
	var kinds []string
	for _, unit := range pluginUnits(output) {
		kind, _, _ := strings.Cut(unit, " ")
		kinds = append(kinds, kind)
	}
	return kinds
}
