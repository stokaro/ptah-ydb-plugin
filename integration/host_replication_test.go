//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// listedNames lists a directory's entries in name order.
func listedNames(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	var names []string
	for _, child := range listing.Children {
		names = append(names, child.Name)
	}
	slices.Sort(names)
	return names
}

// awaitNames lists a directory until it holds want, for up to two minutes:
// YDB's replication controller creates a replica after the statement
// returns.
func awaitNames(c *qt.C, opened *connection.Connection, absolute string, want []string) {
	c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !slices.Equal(listedNames(c, opened, absolute), want) {
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for %v in %s, found %v", want, absolute, listedNames(c, opened, absolute)))
		time.Sleep(time.Second)
	}
}

// hostRun applies text through the host and requires its answer to match
// want, with no comparison limit left.
func hostRun(c *qt.C, directory, name, text, want string) {
	c.Helper()
	output, err := hostApplySource(c, directory, "yaml", text)
	c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
	c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
}

// The host plans an async replication of the server's own table through this
// provider from YAML: it creates the replication, which creates the replica,
// finds nothing to change while the replica table it leaves out of the read
// stands, and drops the replication with its replica once the source no
// longer declares it.
func TestHostAppliesAnAsyncReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		absolute := func(name string) string { return path.Join(database, directory, name) }
		for _, statement := range []string{"DROP ASYNC REPLICATION `" + absolute("mirror") + "` CASCADE", "DROP TABLE `" + absolute("rep") + "`", "DROP TABLE `" + absolute("src") + "`"} {
			_ = execFixtureSchema(cleanup, opened.DB, statement)
		}
	})
	tables := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"
	declared := tables + "async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + path.Join(database, directory, "src") + "\n        target: " + path.Join(directory, "rep") + "\n"

	hostRun(c, directory, "the table", tables, `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "the replication", declared, `(?s).*Schema apply completed successfully.*`)
	awaitNames(c, opened, path.Join(database, directory), []string{"mirror", "rep", "src"})
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
	hostRun(c, directory, "the removal", tables+"async_replications: {}\n", `(?s).*Schema apply completed successfully.*`)
	awaitNames(c, opened, path.Join(database, directory), []string{"src"})
}

// offsetLambda is a transfer lambda that writes each message's offset in the
// changefeed's one partition and the text it carries, with prefix in front.
func offsetLambda(prefix string) string {
	return fmt.Sprintf("($msg) -> { return [<| offset: $msg._offset, message: '%s'u || CAST($msg._data AS Utf8) |>]; }", prefix)
}

// The host plans a transfer from a changefeed into a table through this
// provider from YAML: it creates the transfer reading through a consumer the
// changefeed declares, finds nothing to change, changes the lambda in place
// and drops the transfer once the source no longer declares it.
func TestHostAppliesATransfer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target does not support transfers")
	}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TRANSFER `"+path.Join(database, directory, "ingest")+"`")
		for _, name := range []string{"order_log", "orders"} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+path.Join(database, directory, name)+"`")
		}
	})
	tables := "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n        consumers:\n          reader: {}\n" +
		"  order_log:\n    schema: " + directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n"
	transfer := func(prefix string) string {
		return tables + "transfers:\n  ingest:\n    schema: " + directory + "\n    source: " + path.Join(directory, "orders/feed") + "\n    target: " + path.Join(directory, "order_log") +
			"\n    consumer: reader\n    flush_interval: PT1S\n    using: " + strconv.Quote(offsetLambda(prefix)) + "\n"
	}
	rows := func(prefix string) {
		c.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			var count uint64
			c.Assert(opened.DB.QueryRowContext(c.Context(), fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE StartsWith(message, '%s')", path.Join(database, directory, "order_log"), prefix)).Scan(&count), qt.IsNil)
			if count > 0 {
				return
			}
			c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for a row written through %s", prefix))
			time.Sleep(time.Second)
		}
	}

	hostRun(c, directory, "the tables", tables, `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "the transfer", transfer("a:"), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a second run", transfer("a:"), `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+path.Join(database, directory, "orders")+"` (id) VALUES (1)"), qt.IsNil)
	rows("a:")
	hostRun(c, directory, "a new lambda", transfer("b:"), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a run after the change", transfer("b:"), `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+path.Join(database, directory, "orders")+"` (id) VALUES (2)"), qt.IsNil)
	rows("b:")
	hostRun(c, directory, "the removal", tables+"transfers: {}\n", `(?s).*Schema apply completed successfully.*`)
	c.Assert(strings.Join(listedNames(c, opened, path.Join(database, directory)), ","), qt.Equals, "order_log,orders")
}

// hostReplication is a lock fixture for the host-driven replication tests: the
// test's connection, its directory and database, a provider session that
// reads them back, and the YAML of the source table src, the replica rep and
// the replication mirror of src into rep through the server's own nodes.
type hostReplication struct {
	c         *qt.C
	opened    *connection.Connection
	client    *providerruntime.Client
	session   *protocol.Session
	directory string
	database  string
	mirror    string
}

func newHostReplication(c *qt.C) *hostReplication {
	c.Helper()
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	h := &hostReplication{c: c, opened: opened, client: client, session: session, directory: directory, database: database}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP ASYNC REPLICATION `" + h.absolute("mirror") + "` CASCADE", "DROP TABLE `" + h.absolute("rep") + "`", "DROP TABLE `" + h.absolute("src") + "`"} {
			_ = execFixtureSchema(cleanup, opened.DB, statement)
		}
	})
	h.mirror = "async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + h.absolute("src") + "\n        target: " + path.Join(directory, "rep") + "\n"
	return h
}

// absolute is the absolute path of name in the fixture's directory.
func (h *hostReplication) absolute(name string) string {
	return path.Join(h.database, h.directory, name)
}

// tables is the YAML of the tables named, each keyed on id with a note.
func (h *hostReplication) tables(names ...string) string {
	text := "tables:\n"
	for _, name := range names {
		text += "  " + name + ":\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"
	}
	return text
}

// apply runs the host's schema apply over text and returns its answer.
func (h *hostReplication) apply(text string) (string, error) {
	h.c.Helper()
	return hostApplySource(h.c, h.directory, "yaml", text)
}

// replication reads the replication mirror back through the provider, and
// reports whether the read holds one.
func (h *hostReplication) replication() (replication.ReplicationSpec, string, bool) {
	h.c.Helper()
	snapshot, err := h.client.Inspect(h.c.Context(), &protocol.InspectRequest{SessionId: h.session.Id, Schemas: []string{h.directory}})
	h.c.Assert(err, qt.IsNil)
	value := object(snapshot, schema.ReplicationProperties, "mirror")
	if value == nil {
		return replication.ReplicationSpec{}, "", false
	}
	spec, state, err := schema.ObservedReplication(value)
	h.c.Assert(err, qt.IsNil)
	return spec, state, true
}

// awaitReplica waits, for up to two minutes, until the replication controller
// has created rep and writes it.
func (h *hostReplication) awaitReplica() {
	h.c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !replicaWritten(h.c, h.c.Context(), h.opened, path.Join(h.database, h.directory), "rep") {
		h.c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for the replica rep"))
		time.Sleep(time.Second)
	}
}

// Ported from the host's TestYDBReplication_RefusesATableAtAReplica: a schema
// that declares a table at the path of a replica is refused before anything
// runs, beside the replication that writes the replica, since YDB keeps the
// replica read-only while it runs, and without it, since the drop with CASCADE
// would take the declared table. Each refusal leaves the replication running
// and the replica as it was.
func TestHostRefusesATableAtAReplica_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostReplication(c)
	output, err := h.apply(h.tables("src"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = h.apply(h.tables("src") + h.mirror)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	h.awaitReplica()
	quoted := regexp.QuoteMeta(h.directory)

	kept, keptErr := h.apply(h.tables("src", "rep") + h.mirror)
	dropped, droppedErr := h.apply(h.tables("src", "rep") + "async_replications: {}\n")

	c.Assert(keptErr, qt.IsNotNil)
	c.Assert(kept, qt.Matches, `(?s).*table `+quoted+`/rep: it is a replica table async replication `+quoted+`/mirror writes, read-only while the replication runs `+
		"\\(`path is an async replica table`\\); remove the table from the schema, or fail the replication over first .*")
	c.Assert(droppedErr, qt.IsNotNil)
	c.Assert(dropped, qt.Matches, `(?s).*async replication `+quoted+`/mirror: the schema drops it and declares table `+quoted+`/rep, its replica, `+
		`and YDB keeps a replica of a replication dropped without failover read-only for good .*fail it over first .*`)
	_, state, found := h.replication()
	c.Assert(found, qt.IsTrue)
	c.Assert(state, qt.Equals, replication.StateRunning)
	c.Assert(replicaWritten(c, c.Context(), h.opened, path.Join(h.database, h.directory), "rep"), qt.IsTrue)
}

// Ported from the third half of the host's
// TestYDBReplication_RefusesATableAtAReplica: a replication dropped without
// CASCADE leaves its replica, which YDB keeps read-only for good. The read
// names the replica as a table it does not describe, so the host refuses a
// schema that declares a table at its path before any step, with the reason
// the host's engine gives, and runs nothing; a schema that leaves the table
// out keeps it, and db read names it in a note. The read is filtered to the
// fixture's directory, as every apply here is, and the provider still tells
// the replica from one a replication elsewhere in the database writes. A host
// that does not list schema.unread-tables plans the table, and the provider
// refuses its creation before it runs a statement.
func TestHostRefusesATableAtAnOrphanedReplica_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.UnreadTablesFeature)
	h := newHostReplication(c)
	output, err := h.apply(h.tables("src"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = h.apply(h.tables("src") + h.mirror)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	h.awaitReplica()
	c.Assert(execFixtureSchema(c.Context(), h.opened.DB, "DROP ASYNC REPLICATION `"+h.absolute("mirror")+"`"), qt.IsNil)
	quoted := regexp.QuoteMeta(h.directory)
	reason := quoted + "\\.rep is a replica table no replication of this database writes any more, which YDB keeps read-only for good " +
		"\\(`path is an async replica table`\\); drop it by hand with DROP TABLE, which YDB accepts, or remove it from the schema"

	declared, declaredErr := h.apply(h.tables("src", "rep") + "async_replications: {}\n")
	left, leftErr := h.apply(h.tables("src") + "async_replications: {}\n")
	read, readErr := hostCommand(c, "db", "read", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", h.directory)

	c.Assert(declaredErr, qt.IsNotNil)
	c.Assert(declared, qt.Matches, `(?s).*table `+quoted+`\.rep: `+reason+`\n.*`)
	c.Assert(declared, qt.Not(qt.Contains), "NOT_STARTED")
	c.Assert(leftErr, qt.IsNil, qt.Commentf("%s", left))
	c.Assert(left, qt.Contains, "Schema is synced, no changes to be made")
	c.Assert(readErr, qt.IsNil, qt.Commentf("%s", read))
	c.Assert(read, qt.Matches, `(?s).*note: 1 table this read does not describe: `+quoted+`\.rep: `+reason+`\n.*`)
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.DeepEquals, []string{"rep", "src"})
	c.Assert(replicaWritten(c, c.Context(), h.opened, path.Join(h.database, h.directory), "rep"), qt.IsTrue)
	c.Assert(execFixtureSchema(c.Context(), h.opened.DB, "UPSERT INTO `"+h.absolute("rep")+"` (id, note) VALUES (1, 'written'u)"), qt.ErrorMatches, `(?s).*replicated table.*`)
}

// hostTransfer is a lock fixture for the host-driven transfer tests, on a
// server that runs transfers: the test's connection, its directory and
// database, and a provider session that reads them back.
type hostTransfer struct {
	*hostReplication
}

func newHostTransfer(c *qt.C) *hostTransfer {
	c.Helper()
	h := &hostTransfer{newHostReplication(c)}
	if !h.session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target does not support transfers")
	}
	// A transfer goes before the topic it reads, and the topic waits for the
	// transfer's consumer to go, so the drops run in that order.
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TRANSFER `"+h.absolute("ingest")+"`")
		for cleanup.Err() == nil {
			description, err := h.opened.Driver.Topic().Describe(cleanup, h.absolute("events"))
			if err != nil || len(description.Consumers) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TOPIC `"+h.absolute("events")+"`")
		_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TABLE `"+h.absolute("order_log")+"`")
	})
	return h
}

// newHostWithoutTransfers is [newHostReplication] on a server without
// transfers, such as 25.1 with its default flags.
func newHostWithoutTransfers(c *qt.C) *hostReplication {
	c.Helper()
	h := newHostReplication(c)
	if h.session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target supports transfers")
	}
	return h
}

// orderLog is the YAML of the table order_log a transfer writes, keyed on
// each message's partition and offset.
func (h *hostTransfer) orderLog() string {
	return "tables:\n  order_log:\n    schema: " + h.directory + "\n    primary_key: [partition, offset]\n    columns:\n" +
		"      partition: {type: Uint32, not_null: true}\n      offset: {type: Uint64, not_null: true}\n      message: {type: Utf8}\n"
}

// transfer is the YAML of the transfer ingest from the topic events into
// order_log through lambda, with the further settings given.
func (h *hostTransfer) transfer(lambda, settings string) string {
	return "transfers:\n  ingest:\n    schema: " + h.directory + "\n    source: " + path.Join(h.directory, "events") + "\n    target: " + path.Join(h.directory, "order_log") +
		"\n    using: " + strconv.Quote(lambda) + "\n" + settings
}

// transferRead reads the transfer ingest back through the provider, and
// reports whether the read holds one.
func (h *hostTransfer) transferRead() (replication.TransferSpec, string, bool) {
	h.c.Helper()
	snapshot, err := h.client.Inspect(h.c.Context(), &protocol.InspectRequest{SessionId: h.session.Id, Schemas: []string{h.directory}})
	h.c.Assert(err, qt.IsNil)
	value := object(snapshot, schema.TransferProperties, "ingest")
	if value == nil {
		return replication.TransferSpec{}, "", false
	}
	spec, state, err := schema.ObservedTransfer(value)
	h.c.Assert(err, qt.IsNil)
	return spec, state, true
}

// awaitRunning waits, for up to two minutes, until the transfer runs
// through a consumer.
func (h *hostTransfer) awaitRunning() replication.TransferSpec {
	h.c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		spec, state, found := h.transferRead()
		if found && state == replication.StateRunning && spec.Consumer != "" {
			return spec
		}
		h.c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for the transfer to run, it is %q", state))
		time.Sleep(time.Second)
	}
}

// operations lists the native operations an apply planned, in plan order,
// each as kind and subject.
func operations(output string) []string { return pluginUnits(output) }

// Ported from the host's TestYDBOwnedTransfer_CreatesAfterItsTopicAndDropsBeforeIt:
// a topic with a consumer is created first and a transfer through that
// consumer after the topic and the table it writes, and nothing is left to
// plan once the transfer runs. A schema that leaves both out drops the
// transfer before the topic. YDB accepts DROP TOPIC under a running transfer,
// so only the plan's order keeps the topic from going first.
func TestHostOrdersATransferAroundItsTopic_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTransfer(c)
	topic := "topics:\n  events:\n    schema: " + h.directory + "\n    consumers:\n      ingest: {}\n"
	declared := h.orderLog() + topic + h.transfer(lambdaWriting("a:"), "    consumer: ingest\n    flush_interval: PT1S\n")
	qualified := func(name string) string { return h.directory + "." + name }

	created, err := h.apply(declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	listed := operations(created)
	c.Assert(listed, qt.HasLen, 3, qt.Commentf("%s", created))
	c.Assert(slices.Sorted(slices.Values(listed[:2])), qt.DeepEquals, []string{"row-table-create-operation " + qualified("order_log"), "topic-operation " + qualified("events")},
		qt.Commentf("%s", created))
	c.Assert(listed[2], qt.Equals, "transfer-operation "+qualified("ingest"), qt.Commentf("%s", created))
	spec := h.awaitRunning()
	c.Assert(spec.Consumer, qt.Equals, "ingest")
	settled, err := h.apply(declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", settled))
	c.Assert(settled, qt.Contains, "Schema is synced, no changes to be made")

	removed, err := h.apply(h.orderLog() + "topics: {}\ntransfers: {}\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", removed))
	c.Assert(operations(removed), qt.DeepEquals, []string{"transfer-operation " + qualified("ingest"), "topic-operation " + qualified("events")}, qt.Commentf("%s", removed))
	_, _, found := h.transferRead()
	c.Assert(found, qt.IsFalse)
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.DeepEquals, []string{"order_log"})
	settled, err = h.apply(h.orderLog() + "topics: {}\ntransfers: {}\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", settled))
	c.Assert(settled, qt.Contains, "Schema is synced, no changes to be made")
}

// Ported from the host's TestYDBTransfer_RefusedOn251: a transfer is refused
// on a line without transfers, such as 25.1 with its default flags (`Topic
// transfer creation is disabled`), before any statement runs. The host
// reports the provider's refusal by its status code.
func TestHostRefusesATransferWithoutTheCapability_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostWithoutTransfers(c)
	transfer := &hostTransfer{h}
	declared := transfer.orderLog() + "topics:\n  events:\n    schema: " + h.directory + "\n" + transfer.transfer(lambdaWriting("a:"), "")

	output, err := h.apply(declared)

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "provider compare models failed (InvalidArgument)")
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.HasLen, 0)
}
