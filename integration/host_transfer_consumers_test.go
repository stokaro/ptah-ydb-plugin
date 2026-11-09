//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// adoptionFixture is a lock fixture for the transfer consumer tests, on a
// server that runs transfers: the test's connection, its directory and
// database, and a provider session that reads the transfer back.
type adoptionFixture struct {
	c         *qt.C
	opened    *connection.Connection
	client    *providerruntime.Client
	session   *protocol.Session
	directory string
	database  string
}

func newAdoptionFixture(c *qt.C, source string) *adoptionFixture {
	c.Helper()
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target does not support transfers")
	}
	f := &adoptionFixture{c: c, opened: opened, client: client, session: session, directory: directory, database: database}
	// The transfer goes first, and its source waits until the consumer YDB
	// made for it has gone with it.
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TRANSFER `"+f.absolute("ingest")+"`")
		for cleanup.Err() == nil {
			description, err := opened.Driver.Topic().Describe(cleanup, f.absolute(source))
			if err != nil || len(description.Consumers) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TOPIC `"+f.absolute("events")+"`")
		for _, name := range []string{"order_log", "orders"} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+f.absolute(name)+"`")
		}
	})
	return f
}

func (f *adoptionFixture) absolute(name string) string {
	return path.Join(f.database, f.directory, name)
}

// apply runs the host's schema apply over text and requires want of its
// answer.
func (f *adoptionFixture) apply(name, text, want string) string {
	f.c.Helper()
	output, err := hostApplySource(f.c, f.directory, "yaml", text)
	f.c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	f.c.Assert(output, qt.Contains, want, qt.Commentf("%s: %s", name, output))
	return output
}

// transfer is the YAML of the transfer ingest from source into order_log,
// naming no consumer, so YDB makes one of its own for it.
func (f *adoptionFixture) transfer(source string) string {
	return "transfers:\n  ingest:\n    schema: " + f.directory + "\n    source: " + path.Join(f.directory, source) + "\n    target: " + path.Join(f.directory, "order_log") +
		"\n    flush_interval: PT1S\n    using: " + strconv.Quote(offsetLambda("t:")) + "\n"
}

// orderLog is the YAML of the table order_log the transfer writes.
func (f *adoptionFixture) orderLog() string {
	return "  order_log:\n    schema: " + f.directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n"
}

// consumer waits, for up to two minutes, until the transfer runs through a
// consumer of source, and returns that consumer's name, which YDB made.
func (f *adoptionFixture) consumer(source string) string {
	f.c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		snapshot, err := f.client.Inspect(f.c.Context(), &protocol.InspectRequest{SessionId: f.session.Id, Schemas: []string{f.directory}})
		f.c.Assert(err, qt.IsNil)
		if value := object(snapshot, schema.TransferProperties, "ingest"); value != nil {
			spec, state, err := schema.ObservedTransfer(value)
			f.c.Assert(err, qt.IsNil)
			if state == replication.StateRunning && spec.Consumer != "" {
				f.c.Assert(f.consumers(source), qt.DeepEquals, []string{spec.Consumer})
				return spec.Consumer
			}
		}
		f.c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for the transfer to run through a consumer of %s", source))
		time.Sleep(time.Second)
	}
}

// consumers names the consumers of the topic at source, in name order.
func (f *adoptionFixture) consumers(source string) []string {
	f.c.Helper()
	description, err := f.opened.Driver.Topic().Describe(f.c.Context(), f.absolute(source))
	f.c.Assert(err, qt.IsNil)
	var names []string
	for _, consumer := range description.Consumers {
		names = append(names, consumer.Name)
	}
	slices.Sort(names)
	return names
}

// awaitRow waits, for up to two minutes, until order_log holds a row the
// transfer wrote.
func (f *adoptionFixture) awaitRow() {
	f.c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var count uint64
		f.c.Assert(f.opened.DB.QueryRowContext(f.c.Context(), fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE StartsWith(message, 't:')", f.absolute("order_log"))).Scan(&count), qt.IsNil)
		if count > 0 {
			return
		}
		f.c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for a row written through the transfer"))
		time.Sleep(time.Second)
	}
}

// A transfer from a changefeed that names no consumer reads through one YDB
// makes for it, and the declaration of the changefeed cannot name that
// consumer. The read marks it as the transfer's and the comparison keeps it, as
// the host's embedded engine adopts it: the run after the transfer's creation
// plans nothing, and a change of the changefeed's retention keeps the
// consumer, which a row then still flows through. Without the adoption, the
// run after the creation dropped the consumer under the running transfer,
// measured on 26.2.1.14. The tables go first, as TestHostAppliesATransfer_HappyPath
// applies them.
func TestHostKeepsTheConsumerYDBMadeForATransferOfAChangefeed_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newAdoptionFixture(c, "orders/feed")
	tables := func(retention string) string {
		return "tables:\n  orders:\n    schema: " + f.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
			"    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n        retention_period: " + retention + "\n" + f.orderLog()
	}
	f.apply("the tables", tables("PT6H"), "Schema apply completed successfully")
	f.apply("the transfer", tables("PT6H")+f.transfer("orders/feed"), "Schema apply completed successfully")
	made := f.consumer("orders/feed")

	settled := f.apply("a second run", tables("PT6H")+f.transfer("orders/feed"), "Schema is synced, no changes to be made")
	changed := f.apply("a longer retention", tables("PT12H")+f.transfer("orders/feed"), "Schema apply completed successfully")

	c.Assert(settled, qt.Not(qt.Contains), unitPrefix)
	c.Assert(changed, qt.Not(qt.Contains), "loses its position")
	c.Assert(f.consumers("orders/feed"), qt.DeepEquals, []string{made})
	f.apply("a run after the change", tables("PT12H")+f.transfer("orders/feed"), "Schema is synced, no changes to be made")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "UPSERT INTO `"+f.absolute("orders")+"` (id) VALUES (1)"), qt.IsNil)
	f.awaitRow()
	f.apply("the removal", tables("PT12H")+"transfers: {}\n", "Schema apply completed successfully")
	f.apply("a run after the removal", tables("PT12H")+"transfers: {}\n", "Schema is synced, no changes to be made")
}

// A transfer from a standalone topic that names no consumer reads through one
// YDB makes for it, on a topic declared without consumers. The read marks it as
// the transfer's and the comparison keeps it: the run after the creation plans
// nothing, and a change of the topic's retention keeps the consumer. The change
// runs because the operation compares what it reads before the statement with
// what the plan read without the mark, which a read of the topic alone cannot
// make. Without the adoption, the run after the creation dropped the consumer,
// measured on 26.2.1.14.
func TestHostKeepsTheConsumerYDBMadeForATransferOfATopic_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newAdoptionFixture(c, "events")
	declared := func(retention string) string {
		return "tables:\n" + f.orderLog() + "topics:\n  events:\n    schema: " + f.directory + "\n    retention_period: " + retention + "\n" + f.transfer("events")
	}
	f.apply("the transfer", declared("PT6H"), "Schema apply completed successfully")
	made := f.consumer("events")

	settled := f.apply("a second run", declared("PT6H"), "Schema is synced, no changes to be made")
	changed := f.apply("a longer retention", declared("PT12H"), "Schema apply completed successfully")

	c.Assert(settled, qt.Not(qt.Contains), unitPrefix)
	c.Assert(changed, qt.Not(qt.Contains), "DROP CONSUMER")
	c.Assert(f.consumers("events"), qt.DeepEquals, []string{made})
	f.apply("a run after the change", declared("PT12H"), "Schema is synced, no changes to be made")
	f.apply("the removal", "tables:\n"+f.orderLog()+"topics: {}\ntransfers: {}\n", "Schema apply completed successfully")
	f.apply("a run after the removal", "tables:\n"+f.orderLog()+"topics: {}\ntransfers: {}\n", "Schema is synced, no changes to be made")
}
