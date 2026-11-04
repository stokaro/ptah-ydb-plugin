//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// operationOrder lists the native operations an apply printed, in plan
// order, each as its kind and subject.
func operationOrder(output string) []string {
	var listed []string
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		kind, found := strings.CutPrefix(line, "Native operation: github.com/stokaro/ptah-ydb-plugin/")
		if !found || i+1 >= len(lines) {
			continue
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(lines[i+1]), ":")
		listed = append(listed, kind+" "+subject[strings.LastIndex(subject, " ")+1:])
	}
	return listed
}

// The host plans an async replication and its source table from one YAML
// document: the table is created first and the replication after it, which
// then creates its replica. Created the other way round, the replication reads
// as running with its item and YDB creates no replica for a minute and more,
// measured on 25.1.4.7 and 26.2.1.14. A document that leaves both out drops
// the replication, with its replica, before the table.
func TestHostCreatesAReplicationAfterItsSourceTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP ASYNC REPLICATION `" + absolute("mirror") + "` CASCADE", "DROP TABLE `" + absolute("rep") + "`", "DROP TABLE `" + absolute("src") + "`"} {
			_ = execFixtureSchema(cleanup, opened.DB, statement)
		}
	})
	declared := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n" +
		"async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + absolute("src") + "\n        target: " + path.Join(directory, "rep") + "\n"
	qualified := func(name string) string { return directory + "." + name }

	created, err := hostApplySource(c, directory, "yaml", declared)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(operationOrder(created), qt.DeepEquals, []string{"row-table-create-operation " + qualified("src"), "async-replication-operation " + qualified("mirror")}, qt.Commentf("%s", created))
	awaitNames(c, opened, path.Join(database, directory), []string{"mirror", "rep", "src"})
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
	removed, err := hostApplySource(c, directory, "yaml", "tables: {}\nasync_replications: {}\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", removed))
	c.Assert(operationOrder(removed), qt.DeepEquals, []string{"async-replication-operation " + qualified("mirror"), "table-drop-operation " + qualified("src")}, qt.Commentf("%s", removed))
	awaitNames(c, opened, path.Join(database, directory), nil)
}

// requireTransfers skips a test on a target without transfers.
func requireTransfers(c *qt.C) {
	c.Helper()
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target does not support transfers")
	}
}

// The host plans a transfer, the changefeed it reads and the tables of both
// from one YAML document: the transfer is created after the changefeed. The
// table's changefeeds and the transfer are planned by two services of this
// provider, and the transfer names the changefeed it reads the way the
// changefeed planner names it, which is how the host orders the two. A row
// written to the table then reaches the transfer's table, and nothing is left
// to plan.
func TestHostCreatesATransferAfterItsChangefeed_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	requireTransfers(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TRANSFER `"+absolute("ingest")+"`")
		for _, name := range []string{"order_log", "orders"} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+absolute(name)+"`")
		}
	})
	declared := "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n        consumers:\n          reader: {}\n" +
		"  order_log:\n    schema: " + directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n" +
		"transfers:\n  ingest:\n    schema: " + directory + "\n    source: " + path.Join(directory, "orders/feed") + "\n    target: " + path.Join(directory, "order_log") +
		"\n    consumer: reader\n    flush_interval: PT1S\n    using: " + strconv.Quote(offsetLambda("a:")) + "\n"

	created, err := hostApplySource(c, directory, "yaml", declared)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	order := operationOrder(created)
	c.Assert(order, qt.HasLen, 4, qt.Commentf("%s", created))
	c.Assert(order[3], qt.Equals, "transfer-operation "+directory+".ingest", qt.Commentf("%s", created))
	c.Assert(slices.Index(order, "changefeed-operation "+directory+".orders") >= 0, qt.IsTrue, qt.Commentf("%s", created))
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute("orders")+"` (id) VALUES (1)"), qt.IsNil)
	awaitTransferredRow(c, opened, absolute("order_log"), "a:")
}

// awaitTransferredRow waits, for up to two minutes, until table holds a row
// whose message starts with prefix: a transfer writes on its flush interval.
func awaitTransferredRow(c *qt.C, opened *connection.Connection, table, prefix string) {
	c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var count uint64
		c.Assert(opened.DB.QueryRowContext(c.Context(), fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE StartsWith(message, '%s')", table, prefix)).Scan(&count), qt.IsNil)
		if count > 0 {
			return
		}
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for a row written through %s", prefix))
		time.Sleep(time.Second)
	}
}

// Ported from the host's TestYDBTransfer_ASecretTakesTheTopicsPath: a schema
// that removes a transfer and the topic it reads and declares a secret at the
// topic's path drops the transfer, then the topic, then creates the secret,
// and the server runs it. The three are planned by three services of this
// provider. Read by the topic's path, the transfer's drop would read the
// secret created there, and the host ordered it last, after YDB had already
// dropped the topic under it, measured on 26.2.1.14.
func TestHostDropsATransferBeforeTheTopicASecretReplaces_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	requireTransfers(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	variable := "PTAH_SECRET_TRANSFER_EVENTS_" + rand.Text()
	t.Setenv(variable, "probe")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TRANSFER `"+absolute("ingest")+"`")
		for cleanup.Err() == nil {
			description, err := opened.Driver.Topic().Describe(cleanup, absolute("events"))
			if err != nil || len(description.Consumers) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TOPIC `"+absolute("events")+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP SECRET `"+absolute("events")+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+absolute("order_log")+"`")
	})
	table := "tables:\n  order_log:\n    schema: " + directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n"
	held := table + "topics:\n  events:\n    schema: " + directory + "\n    consumers:\n      ingest: {}\n" +
		"transfers:\n  ingest:\n    schema: " + directory + "\n    source: " + path.Join(directory, "events") + "\n    target: " + path.Join(directory, "order_log") +
		"\n    consumer: ingest\n    using: " + strconv.Quote(offsetLambda("t:")) + "\n"
	declared := table + "topics: {}\ntransfers: {}\nsecrets:\n  events:\n    schema: " + directory + "\n    value_env: " + variable + "\n"
	hostRun(c, directory, "the transfer", held, `(?s).*Schema apply completed successfully.*`)
	qualified := func(name string) string { return directory + "." + name }

	planned, err := hostApplySource(c, directory, "yaml", declared)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", planned))
	c.Assert(operationOrder(planned), qt.DeepEquals, []string{"transfer-operation " + qualified("ingest"), "topic-operation " + qualified("events"),
		"secret-operation " + qualified("events")}, qt.Commentf("%s", planned))
	hostRun(c, directory, "a run after the swap", declared, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"events", "order_log"})
}
