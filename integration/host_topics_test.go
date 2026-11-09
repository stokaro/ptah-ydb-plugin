//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	topicspec "github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// topicShape is what the server holds of a topic: the settings the fixtures
// declare and each consumer, as its name and whether it is important.
type topicShape struct {
	MinActivePartitions int64
	Retention           time.Duration
	Consumers           []string
}

// describedTopic reads a topic back through the topic service, or reports
// that its directory holds no entry of that name.
func describedTopic(c *qt.C, opened *connection.Connection, database, directory, name string) (topicShape, bool) {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	found := false
	for _, entry := range listing.Children {
		found = found || entry.Name == name
	}
	if !found {
		return topicShape{}, false
	}
	description, err := opened.Driver.Topic().Describe(c.Context(), path.Join(database, directory, name))
	c.Assert(err, qt.IsNil)
	shape := topicShape{MinActivePartitions: description.PartitionSettings.MinActivePartitions, Retention: description.RetentionPeriod}
	for _, consumer := range description.Consumers {
		label := consumer.Name
		if consumer.Important {
			label += " important"
		}
		shape.Consumers = append(shape.Consumers, label)
	}
	slices.Sort(shape.Consumers)
	return shape, true
}

// The host plans a topic through this provider from each source that
// declares one: it creates the topic with its consumers, finds nothing to
// change on the next run, changes the retention and the consumers in place,
// and drops the topic once the source no longer declares it. Every source
// describes the topics of the directory completely.
func TestHostAppliesATopic_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format                 string
		created, changed, gone string
	}{
		{format: "yaml",
			created: "topics:\n  events:\n    schema: {dir}\n    min_active_partitions: 2\n    retention_period: PT36H\n    consumers:\n      billing:\n        important: true\n      audit: {}\n",
			changed: "topics:\n  events:\n    schema: {dir}\n    min_active_partitions: 2\n    retention_period: P2D\n    consumers:\n      billing: {}\n      fresh: {}\n",
			gone:    "topics: {}\n"},
		{format: "go",
			created: "package entities\n\n" + goItems + "\n//ptah:schema:topic name=\"events\" schema=\"{dir}\" min_active_partitions=\"2\" retention_period=\"PT36H\"\n" +
				"//ptah:schema:topic:consumer name=\"billing\" topic=\"events\" schema=\"{dir}\" important=\"true\"\n//ptah:schema:topic:consumer name=\"audit\" topic=\"events\" schema=\"{dir}\"\ntype Events struct{}\n",
			changed: "package entities\n\n" + goItems + "\n//ptah:schema:topic name=\"events\" schema=\"{dir}\" min_active_partitions=\"2\" retention_period=\"P2D\"\n" +
				"//ptah:schema:topic:consumer name=\"billing\" topic=\"events\" schema=\"{dir}\"\n//ptah:schema:topic:consumer name=\"fresh\" topic=\"events\" schema=\"{dir}\"\ntype Events struct{}\n",
			gone: "package entities\n\n" + goItems},
		{format: "sql",
			created: sqlItems + "CREATE TOPIC `{dir}/events` (CONSUMER billing WITH (important = TRUE), CONSUMER audit) WITH (min_active_partitions = 2, retention_period = Interval('PT36H'));\n",
			changed: sqlItems + "CREATE TOPIC `{dir}/events` (CONSUMER billing, CONSUMER fresh) WITH (min_active_partitions = 2, retention_period = Interval('P2D'));\n",
			gone:    sqlItems},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = execFixtureSchema(cleanup, opened.DB, "DROP TOPIC `"+path.Join(database, directory, "events")+"`")
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
			})
			apply := func(name, text, want string) {
				c.Helper()
				output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(text, "{dir}", directory))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
			}
			apply("creation", test.created, `(?s).*Schema apply completed successfully.*`)
			shape, found := describedTopic(c, opened, database, directory, "events")
			c.Assert(found, qt.IsTrue)
			c.Assert(shape, qt.DeepEquals, topicShape{MinActivePartitions: 2, Retention: 36 * time.Hour, Consumers: []string{"audit", "billing important"}})
			apply("a second run", test.created, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a change", test.changed, `(?s).*Schema apply completed successfully.*`)
			shape, found = describedTopic(c, opened, database, directory, "events")
			c.Assert(found, qt.IsTrue)
			c.Assert(shape, qt.DeepEquals, topicShape{MinActivePartitions: 2, Retention: 48 * time.Hour, Consumers: []string{"billing", "fresh"}})
			apply("a run after the change", test.changed, `(?s).*Schema is synced, no changes to be made.*`)
			apply("the removal", test.gone, `(?s).*Schema apply completed successfully.*`)
			_, found = describedTopic(c, opened, database, directory, "events")
			c.Assert(found, qt.IsFalse)
		})
	}
}

// hostTopics is a lock fixture for the host-driven topic ports: the test's
// connection, its directory and database, and a provider session that reads
// the directory's topics back.
type hostTopics struct {
	*hostChangefeeds
}

func newHostTopics(c *qt.C) *hostTopics {
	c.Helper()
	h := &hostTopics{newHostChangefeeds(c)}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"orders", "plain", "swap"} {
			_ = execFixtureSchema(cleanup, h.opened.DB, "DROP TOPIC `"+path.Join(h.database, h.directory, name)+"`")
		}
		for _, name := range []string{"notes", "swap"} {
			c.Check(execFixtureSchema(cleanup, h.opened.DB, "DROP TABLE IF EXISTS `"+path.Join(h.database, h.directory, name)+"`"), qt.IsNil)
		}
	})
	return h
}

// declared is the YAML of table notes beside the topics block given, so a
// read of the directory is not empty for the topics alone.
func (h *hostTopics) declared(topics string) string {
	return "tables:\n  notes:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\ntopics:\n" + topics
}

// topic reads topic name back through the provider.
func (h *hostTopics) topic(name string) ydbdecl.Topic {
	h.c.Helper()
	snapshot, err := h.client.Inspect(h.c.Context(), &protocol.InspectRequest{SessionId: h.session.Id, Schemas: []string{h.directory}})
	h.c.Assert(err, qt.IsNil)
	value := object(snapshot, schema.TopicProperties, name)
	h.c.Assert(value, qt.IsNotNil, qt.Commentf("topic %s is not in the read", name))
	observed, err := schema.ObservedTopic(value)
	h.c.Assert(err, qt.IsNil)
	read, err := topicspec.FromObserved(observed)
	h.c.Assert(err, qt.IsNil)
	return read
}

// fullTopic declares a topic naming every setting and two consumers, one
// with an availability period on a line that takes one, as YAML and as the
// read takes it, with the consumers in the order YDB keeps them: the order
// the topic was created with.
func fullTopic(capabilities map[string]bool) (string, ydbdecl.Topic) {
	audit := ydbdecl.Consumer{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "gzip"}}
	auditText := "        audit: {read_from: \"2026-01-01T00:00:00Z\", supported_codecs: \"raw,gzip\"}\n"
	if capabilities["topic_consumer_availability_period"] {
		audit.AvailabilityPeriod = "PT2H"
		auditText = "        audit: {read_from: \"2026-01-01T00:00:00Z\", supported_codecs: \"raw,gzip\", availability_period: PT2H}\n"
	}
	text := "  orders:\n    schema: {dir}\n    min_active_partitions: 2\n    max_active_partitions: 6\n    auto_partitioning_strategy: scale_up\n" +
		"    auto_partitioning_up_utilization_percent: 70\n    auto_partitioning_down_utilization_percent: 10\n    auto_partitioning_stabilization_window: PT2M\n" +
		"    retention_period: PT36H\n    partition_write_speed_bytes_per_second: 2097152\n    partition_write_burst_bytes: 3145728\n    supported_codecs: \"raw,gzip\"\n" +
		"    consumers:\n      billing: {important: true}\n" + strings.ReplaceAll(auditText, "        ", "      ")
	return text, ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 6, AutoPartitioningStrategy: "scale_up",
		AutoPartitioningUpUtilizationPercent: 70, AutoPartitioningDownUtilizationPercent: 10, AutoPartitioningStabilizationWindow: "PT2M", RetentionPeriod: "P1DT12H",
		PartitionWriteSpeedBytesPerSecond: 2097152, PartitionWriteBurstBytes: 3145728, SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true}, audit}}
}

// Ported from the host's TestYDBTopics_RoundTrip: a topic naming every setting
// and a topic naming none are created, read back with the settings declared
// and, for the plain one, the ones YDB gives a new topic, and the runs after
// the first plan nothing. The CREATE TOPIC statement is pinned by
// TestStatements in internal/topics.
func TestHostAppliesEveryTopicSetting_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTopics(c)
	text, want := fullTopic(h.session.Capabilities)
	declared := h.declared(strings.ReplaceAll(text, "{dir}", h.directory) + "  plain:\n    schema: " + h.directory + "\n")

	h.apply("creation", declared, "Schema apply completed successfully")
	h.apply("a second run", declared, "Schema is synced, no changes to be made")
	h.apply("a third run", declared, "Schema is synced, no changes to be made")

	c.Assert(h.topic("orders"), qt.DeepEquals, want)
	c.Assert(h.topic("plain"), qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 1, AutoPartitioningStrategy: "disabled", RetentionPeriod: "P1D",
		PartitionWriteSpeedBytesPerSecond: 1048576, PartitionWriteBurstBytes: 1048576})
}

// Ported from the host's TestYDBTopics_ChangesConverge: a topic's settings and
// consumers change in one plan, and one apply leaves nothing to plan. The
// changes are the ones whose naive statement would leave the topic short of
// its declaration: a write speed changed alone, which leaves the burst where
// the topic was created with it; auto-partitioning enabled on a topic created
// without it, which takes thresholds of 80% and 20% rather than the 90% and
// 30% a topic created with it gets; a consumer whose codec list is emptied,
// which YDB changes only by dropping the consumer and adding it again; and
// consumers added, changed and dropped together.
func TestHostConvergesTopicChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTopics(c)
	before := "  orders:\n    schema: " + h.directory + "\n    partition_write_speed_bytes_per_second: 2097152\n" +
		"    consumers:\n      gone: {}\n      kept: {important: true}\n      narrowed: {supported_codecs: raw}\n"
	after := "  orders:\n    schema: " + h.directory + "\n    min_active_partitions: 2\n    max_active_partitions: 4\n    auto_partitioning_strategy: scale_up\n" +
		"    retention_period: PT2H\n    partition_write_speed_bytes_per_second: 4194304\n" +
		"    consumers:\n      kept: {read_from: \"2026-01-01T00:00:00Z\"}\n      narrowed: {}\n      fresh: {important: true}\n"
	h.apply("the topic", h.declared(before), "Schema apply completed successfully")

	h.apply("the changes", h.declared(after), "Schema apply completed successfully")

	h.apply("a run after the changes", h.declared(after), "Schema is synced, no changes to be made")
	c.Assert(h.topic("orders"), qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 4, AutoPartitioningStrategy: "scale_up",
		AutoPartitioningUpUtilizationPercent: 90, AutoPartitioningDownUtilizationPercent: 30, AutoPartitioningStabilizationWindow: "PT5M", RetentionPeriod: "PT2H",
		PartitionWriteSpeedBytesPerSecond: 4194304, PartitionWriteBurstBytes: 4194304,
		// YDB keeps the consumers in the order they were added: kept stays,
		// fresh is added, and narrowed is dropped and added again after it.
		Consumers: []ydbdecl.Consumer{{Name: "kept", ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "fresh", Important: true}, {Name: "narrowed"}}})
}

// Ported from the host's TestYDBTopics_ATopicAndATableTradeAPath: a table is
// planned into the path a dropped topic held, and a topic into the path a
// dropped table held. YDB keeps one object at a path (`unexpected path
// type`), so a plan that created before it dropped would fail at the create;
// each plan drops first, and nothing is left to plan after it.
func TestHostTradesAPathBetweenATopicAndATable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTopics(c)
	asTopic := h.declared("  swap:\n    schema: " + h.directory + "\n")
	asTable := "tables:\n  notes:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"  swap:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\ntopics: {}\n"
	swap := h.directory + ".swap"
	h.apply("the topic", asTopic, "Schema apply completed successfully")

	toTable := h.apply("the table", asTable, "Schema apply completed successfully")
	tableSettled := h.apply("a run after the table", asTable, "Schema is synced, no changes to be made")
	toTopic := h.apply("the topic again", asTopic, "Schema apply completed successfully")
	topicSettled := h.apply("a run after the topic", asTopic, "Schema is synced, no changes to be made")

	c.Assert(operations(toTable), qt.DeepEquals, []string{"topic-operation " + swap, "row-table-create-operation " + swap}, qt.Commentf("%s", toTable))
	c.Assert(toTable, qt.Contains, swap+": DROP TOPIC removes the topic")
	c.Assert(tableSettled, qt.Not(qt.Contains), unitPrefix)
	c.Assert(operations(toTopic), qt.DeepEquals, []string{"table-drop-operation " + swap, "topic-operation " + swap}, qt.Commentf("%s", toTopic))
	c.Assert(toTopic, qt.Contains, swap+": Drops a table and its rows")
	c.Assert(topicSettled, qt.Not(qt.Contains), unitPrefix)
	h.topic("swap")
}
