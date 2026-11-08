//go:build integration

package integration_test

import (
	"context"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// changefeedTransfer is the YAML of the table orders with the changefeed
// feed and its consumer reader, the table order_log, and the transfer ingest
// of the changefeed into order_log through that consumer.
func changefeedTransfer(h *hostTransfer) string {
	return "tables:\n  orders:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n        consumers:\n          reader: {}\n" +
		"  order_log:\n    schema: " + h.directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n" +
		"transfers:\n  ingest:\n    schema: " + h.directory + "\n    source: " + path.Join(h.directory, "orders/feed") + "\n    target: " + path.Join(h.directory, "order_log") +
		"\n    consumer: reader\n    flush_interval: PT1S\n    using: " + strconv.Quote(offsetLambda("a:")) + "\n"
}

// A transfer the schema keeps, whose changefeed was dropped by hand while the
// transfer stayed, is not reported as synced because its own definition did
// not change. The read finds the declared changefeed missing from orders, and
// the plan adds it back with its consumer, in one changefeed operation, and
// plans nothing for the transfer. After the apply nothing is left to plan.
func TestHostRestoresTheChangefeedAKeptTransferReads_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTransfer(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, h.opened.DB, "DROP TABLE `"+h.absolute("orders")+"`")
	})
	declared := changefeedTransfer(h)
	hostRun(c, h.directory, "the schema", declared, `(?s).*Schema apply completed successfully.*`)
	c.Assert(execFixtureSchema(c.Context(), h.opened.DB, "ALTER TABLE `"+h.absolute("orders")+"` DROP CHANGEFEED `feed`"), qt.IsNil)

	planned := hostPlanned(c, h.directory, "yaml", declared)
	hostRun(c, h.directory, "the restoring run", declared, `(?s).*Schema apply completed successfully.*`)

	c.Assert(planned, qt.DeepEquals, []string{"changefeed-operation " + h.directory + ".orders"})
	c.Assert(hostPlanned(c, h.directory, "yaml", declared), qt.HasLen, 0)
	_, _, found := h.transferRead()
	c.Assert(found, qt.IsTrue)
}

// outOfScopeTransfer creates the topic events in a directory beside the
// transfer's, which a run that selects the transfer's directory does not
// read, and returns that directory and the schema of a transfer that reads
// the topic into a declared table.
func outOfScopeTransfer(c *qt.C, h *hostTransfer) (directory, declared string) {
	c.Helper()
	_, directory, _ = inspectionFixture(c)
	topic := path.Join(h.database, directory, "events")
	c.Assert(execFixtureSchema(c.Context(), h.opened.DB, "CREATE TOPIC `"+topic+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, h.opened.DB, "DROP TOPIC `"+topic+"`")
	})
	return directory, h.orderLog() + "transfers:\n  ingest:\n    schema: " + h.directory + "\n    source: " + path.Join(directory, "events") +
		"\n    target: " + path.Join(h.directory, "order_log") + "\n    using: " + strconv.Quote(lambdaWriting("a:")) + "\n"
}

// A transfer whose topic is outside the directories a run selects is refused:
// the read covers only the selected directory, so it knows nothing of the
// topic, and the comparison does not assume a topic it did not read exists.
// The topic is there, in another directory; declaring it, or selecting its
// directory too, is what the refusal asks for. Nothing is created.
func TestHostRefusesATransferOfATopicOutsideTheScope_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DeclaredObjectsFeature)
	h := newHostTransfer(c)
	other, declared := outOfScopeTransfer(c, h)

	refused, err := h.apply(declared)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "transfer "+h.directory+"/ingest: it reads topic "+other+"/events, which the schema declares neither as a "+
		"topic nor as a changefeed")
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.HasLen, 0)
}

// A host that sends the current context (models.current-context) says which
// directories the read covered, so the refusal of a transfer whose topic is
// outside them says the topic is in a directory the run does not read and
// asks for it to be selected, rather than saying the plan leaves no such
// topic: the topic is there, and the plan would not touch it.
func TestHostRefusesATransferOfATopicOutsideTheScopeAsUnread_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.CurrentContextFeature)
	h := newHostTransfer(c)
	other, declared := outOfScopeTransfer(c, h)

	refused, err := h.apply(declared)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "transfer "+h.directory+"/ingest: it reads topic "+other+"/events, which the schema declares neither as a "+
		"topic nor as a changefeed, and which is in a directory this run does not read, so the plan cannot tell whether it is there")
	c.Assert(refused, qt.Not(qt.Contains), "the plan leaves no such topic")
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.HasLen, 0)
}
