//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topicoptions"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// recordedChangefeed is a transfer fixture over the table orders, whose
// changefeed feed the read cannot describe: YDB keeps a consumer with custom
// codec 10042, which no declaration names. orders is the YAML of the table
// with no changefeed, transfer that of the transfer ingest of feed into
// order_log.
type recordedChangefeed struct {
	*hostTransfer
	orders, transfer string
}

// newRecordedChangefeed creates orders with the changefeed feed, gives the
// changefeed's topic the consumer with the custom codec, and creates
// order_log. The transfer is the caller's to create.
func newRecordedChangefeed(c *qt.C) *recordedChangefeed {
	c.Helper()
	requireHostNegotiates(c, provider.CurrentContextFeature)
	h := &recordedChangefeed{hostTransfer: newHostTransfer(c)}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TRANSFER `"+h.absolute("ingest")+"`")
		_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TABLE `"+h.absolute("orders")+"`")
	})
	h.orders = "  orders:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"
	h.transfer = "transfers:\n  ingest:\n    schema: " + h.directory + "\n    source: " + path.Join(h.directory, "orders/feed") +
		"\n    target: " + path.Join(h.directory, "order_log") + "\n    flush_interval: PT1S\n    using: " + strconv.Quote(lambdaWriting("a:")) + "\n"
	feed := h.orders + "    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n"
	created, err := h.apply(h.orderLog() + feed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(h.opened.Driver.Topic().Alter(c.Context(), h.absolute("orders")+"/feed",
		topicoptions.AlterWithAddConsumers(topictypes.Consumer{Name: "custom", SupportedCodecs: []topictypes.Codec{10042}})), qt.IsNil)
	return h
}

// A new transfer of a changefeed the read cannot describe, under a table that
// declares no changefeed, is created and runs: the plan neither drops nor
// changes such a changefeed, as the host's embedded engine planned it (G239).
// The host sends the changefeeds the read holds under models.current-context;
// a host that does not refuses the transfer as one of a missing topic.
// Measured with built-in 74de0b65b on 26.2.1.14: CREATE TRANSFER, with a
// warning that the changefeed could not be compared.
func TestHostCreatesATransferOfAChangefeedItCannotDescribe_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newRecordedChangefeed(c)

	created, err := h.apply(h.orderLog() + h.orders + h.transfer)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(operations(created), qt.DeepEquals, []string{"transfer-operation " + h.directory + ".ingest"}, qt.Commentf("%s", created))
	c.Assert(h.awaitRunning().Source, qt.Equals, path.Join(h.directory, "orders/feed"))
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.DeepEquals, []string{"ingest", "order_log", "orders"})
}

// A schema that keeps such a transfer, beside a table that declares no
// changefeed, plans nothing for it: the transfer keeps running and the
// changefeed stays. Measured with built-in 74de0b65b on 26.2.1.14: no changes,
// with a warning that the changefeed could not be compared.
func TestHostKeepsATransferOfAChangefeedItCannotDescribe_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newRecordedChangefeed(c)
	created, err := h.apply(h.orderLog() + h.orders + h.transfer)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	h.awaitRunning()

	kept, err := h.apply(h.orderLog() + h.orders + h.transfer)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", kept))
	c.Assert(operations(kept), qt.HasLen, 0, qt.Commentf("%s", kept))
	c.Assert(h.awaitRunning().Source, qt.Equals, path.Join(h.directory, "orders/feed"))
	description, err := h.opened.Driver.Topic().Describe(c.Context(), h.absolute("orders")+"/feed")
	c.Assert(err, qt.IsNil)
	c.Assert(slices.ContainsFunc(description.Consumers, func(consumer topictypes.Consumer) bool { return consumer.Name == "custom" }), qt.IsTrue)
}
