//go:build integration

package integration_test

import (
	"context"
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
