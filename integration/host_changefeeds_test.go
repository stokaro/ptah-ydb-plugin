//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// feedShape is what the server holds of one changefeed: its mode, its topic's
// retention, and each consumer, as its name and whether it is important.
type feedShape struct {
	Mode      options.ChangefeedMode
	Retention time.Duration
	Consumers []string
}

// describedChangefeeds reads the changefeeds of a table back through the
// table and topic services, by name, or reports that the table is absent.
func describedChangefeeds(c *qt.C, opened *connection.Connection, absolute string) (map[string]feedShape, bool) {
	c.Helper()
	var described options.Description
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, s table.Session) error {
		var err error
		described, err = s.DescribeTable(ctx, absolute)
		return err
	}, table.WithIdempotent())
	if err != nil {
		return nil, false
	}
	result := make(map[string]feedShape)
	for _, feed := range described.Changefeeds {
		topic, err := opened.Driver.Topic().Describe(c.Context(), path.Join(absolute, feed.Name))
		c.Assert(err, qt.IsNil)
		shape := feedShape{Mode: feed.Mode, Retention: topic.RetentionPeriod}
		for _, consumer := range topic.Consumers {
			label := consumer.Name
			if consumer.Important {
				label += " important"
			}
			shape.Consumers = append(shape.Consumers, label)
		}
		slices.Sort(shape.Consumers)
		result[feed.Name] = shape
	}
	return result, true
}

// The host plans the changefeeds of a table through this provider from each
// source that declares them: it creates the table and adds its changefeeds
// with their consumers, finds nothing to change on the next run, changes a
// topic's retention and consumers in place and drops and adds a changefeed
// whose mode changes, and drops the changefeeds once the source declares the
// table without them. Every source describes the changefeeds of each table it
// declares completely.
func TestHostAppliesChangefeeds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format                 string
		created, changed, gone string
	}{
		{format: "yaml",
			created: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n    changefeeds:\n" +
				"      updates:\n        mode: UPDATES\n        format: JSON\n        retention_period: PT12H\n        consumers:\n          billing: {important: true}\n          audit: {}\n" +
				"      keys:\n        mode: KEYS_ONLY\n        format: JSON\n",
			changed: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n    changefeeds:\n" +
				"      updates:\n        mode: UPDATES\n        format: JSON\n        retention_period: P2D\n        consumers:\n          billing: {}\n          fresh: {}\n" +
				"      keys:\n        mode: NEW_IMAGE\n        format: JSON\n",
			gone: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n"},
		{format: "go",
			created: "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\"\n" +
				"//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\" retention_period=\"PT12H\"\n" +
				"//ptah:schema:changefeed:consumer name=\"billing\" changefeed=\"updates\" important=\"true\"\n" +
				"//ptah:schema:changefeed:consumer name=\"audit\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed name=\"keys\" mode=\"KEYS_ONLY\" format=\"JSON\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n}\n",
			changed: "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\"\n" +
				"//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\" retention_period=\"P2D\"\n" +
				"//ptah:schema:changefeed:consumer name=\"billing\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed:consumer name=\"fresh\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed name=\"keys\" mode=\"NEW_IMAGE\" format=\"JSON\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n}\n",
			gone: "package entities\n\n" + goItems},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
			})
			apply := func(name, text, want string) {
				c.Helper()
				output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(text, "{dir}", directory))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
			}
			apply("creation", test.created, `(?s).*Schema apply completed successfully.*`)
			shapes, found := describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
				"updates": {Mode: options.ChangefeedModeUpdates, Retention: 12 * time.Hour, Consumers: []string{"audit", "billing important"}},
				"keys":    {Mode: options.ChangefeedModeKeysOnly, Retention: 24 * time.Hour},
			})
			apply("a second run", test.created, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a change", test.changed, `(?s).*Schema apply completed successfully.*`)
			shapes, found = describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
				"updates": {Mode: options.ChangefeedModeUpdates, Retention: 48 * time.Hour, Consumers: []string{"billing", "fresh"}},
				"keys":    {Mode: options.ChangefeedModeNewImage, Retention: 24 * time.Hour},
			})
			apply("a run after the change", test.changed, `(?s).*Schema is synced, no changes to be made.*`)
			apply("the removal", strings.ReplaceAll(test.gone, "{dir}", directory), `(?s).*Schema apply completed successfully.*`)
			shapes, found = describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.HasLen, 0)
			apply("a run after the removal", test.gone, `(?s).*Schema is synced, no changes to be made.*`)
		})
	}
}

// hostChangefeeds is a lock fixture for the host-driven changefeed ports: the
// test's connection, its directory and database, and a provider session that
// reads the table events back.
type hostChangefeeds struct {
	c         *qt.C
	opened    *connection.Connection
	client    *providerruntime.Client
	session   *protocol.Session
	directory string
	database  string
}

func newHostChangefeeds(c *qt.C) *hostChangefeeds {
	c.Helper()
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	return &hostChangefeeds{c: c, opened: opened, client: client, session: session, directory: directory, database: database}
}

// events is the YAML of table events, keyed on a Uint64 so its changefeeds'
// topics may start with several partitions, with the changefeeds block given.
func (h *hostChangefeeds) events(block string) string {
	return "tables:\n  events:\n    schema: " + h.directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      payload: {type: Utf8}\n" + block
}

// apply runs the host's schema apply over text and requires want of its
// answer.
func (h *hostChangefeeds) apply(name, text, want string) string {
	h.c.Helper()
	output, err := hostApplySource(h.c, h.directory, "yaml", text)
	h.c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	h.c.Assert(output, qt.Contains, want, qt.Commentf("%s: %s", name, output))
	return output
}

// read reads the changefeeds of table events back through the provider, in
// name order.
func (h *hostChangefeeds) read() []ydbdecl.Changefeed {
	h.c.Helper()
	snapshot, err := h.client.Inspect(h.c.Context(), &protocol.InspectRequest{SessionId: h.session.Id, Schemas: []string{h.directory}})
	h.c.Assert(err, qt.IsNil)
	var feeds []ydbdecl.Changefeed
	for _, observed := range sourceChangefeeds(h.c, snapshot, "events") {
		read, err := changefeeds.FromObserved(observed)
		h.c.Assert(err, qt.IsNil)
		feeds = append(feeds, read.Changefeed)
	}
	slices.SortFunc(feeds, func(a, b ydbdecl.Changefeed) int { return strings.Compare(a.Name, b.Name) })
	return feeds
}

// lineChangefeeds are the changefeeds the round trip declares on the
// session's line, as YAML and as the read takes them: every option the line
// takes, and consumers, one of them important and reading two codecs from a
// point in time. Debezium JSON writes no timestamps and no schema change
// records, so those options are declared on the JSON changefeed. Where the
// line has them, the options that came later -- user SIDs, schema changes, an
// auto-partitioned topic and a consumer's availability period -- are declared
// too.
func lineChangefeeds(capabilities map[string]bool) (string, []ydbdecl.Changefeed) {
	audit := ydbdecl.Changefeed{Name: "audit", Mode: "NEW_AND_OLD_IMAGES", Format: "DEBEZIUM_JSON", RetentionPeriod: "PT12H", InitialScan: true, TopicMinActivePartitions: 2,
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "gzip"}}, {Name: "search"}}}
	keys := ydbdecl.Changefeed{Name: "keys", Mode: "KEYS_ONLY", Format: "JSON", VirtualTimestamps: true, ResolvedTimestamps: "PT10S",
		UserSIDs: capabilities[changefeeds.CapabilityUserSIDs], SchemaChanges: capabilities[changefeeds.CapabilitySchemaChanges],
		TopicAutoPartitioning: capabilities[changefeeds.CapabilityTopicAutoPartitioning]}
	text := "    changefeeds:\n      audit:\n        mode: NEW_AND_OLD_IMAGES\n        format: DEBEZIUM_JSON\n        retention_period: PT12H\n        initial_scan: true\n" +
		"        topic_min_active_partitions: 2\n        consumers:\n          billing: {important: true, read_from: \"2026-01-01T00:00:00Z\", supported_codecs: \"raw,gzip\"}\n" +
		"          search: {}\n      keys:\n        mode: KEYS_ONLY\n        format: JSON\n        virtual_timestamps: true\n        resolved_timestamps: PT10S\n" +
		fmt.Sprintf("        user_sids: %t\n        schema_changes: %t\n        topic_auto_partitioning: %t\n", keys.UserSIDs, keys.SchemaChanges, keys.TopicAutoPartitioning)
	if capabilities["topic_consumer_availability_period"] {
		keys.Consumers = []ydbdecl.Consumer{{Name: "late", AvailabilityPeriod: "PT1H"}}
		text += "        consumers:\n          late: {availability_period: PT1H}\n"
	}
	return text, []ydbdecl.Changefeed{audit, keys}
}

// Ported from the host's TestYDBChangefeeds_RoundTrip: a table whose
// changefeeds use every option the line takes is created with them, they read
// back from DescribeTable and from DescribeTopic on each changefeed's path as
// declared, and the runs after the first plan nothing.
func TestHostAppliesEveryChangefeedOption_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostChangefeeds(c)
	text, want := lineChangefeeds(h.session.Capabilities)

	h.apply("creation", h.events(text), "Schema apply completed successfully")
	h.apply("a second run", h.events(text), "Schema is synced, no changes to be made")
	h.apply("a third run", h.events(text), "Schema is synced, no changes to be made")

	c.Assert(h.read(), qt.DeepEquals, want)
}

// Ported from the host's TestYDBChangefeeds_SwapAtLimit: a table at YDB's
// limit of five changefeeds swaps one for another whose name sorts before
// it. The changefeed goes before its replacement is added, since adding first
// would pass the limit.
func TestHostSwapsAChangefeedAtTheLimit_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostChangefeeds(c)
	block := func(names ...string) string {
		text := "    changefeeds:\n"
		for _, name := range names {
			text += "      " + name + ":\n        mode: UPDATES\n        format: JSON\n"
		}
		return text
	}
	names := func(feeds []ydbdecl.Changefeed) []string {
		var listed []string
		for _, feed := range feeds {
			listed = append(listed, feed.Name)
		}
		return listed
	}
	h.apply("five changefeeds", h.events(block("b0", "b1", "b2", "b3", "z_old")), "Schema apply completed successfully")
	c.Assert(names(h.read()), qt.DeepEquals, []string{"b0", "b1", "b2", "b3", "z_old"})

	h.apply("the swap", h.events(block("b0", "b1", "b2", "b3", "a_new")), "Schema apply completed successfully")

	c.Assert(names(h.read()), qt.DeepEquals, []string{"a_new", "b0", "b1", "b2", "b3"})
	h.apply("a run after the swap", h.events(block("b0", "b1", "b2", "b3", "a_new")), "Schema is synced, no changes to be made")
}

// Ported from the host's TestYDBChangefeeds_ChangesInPlace: what YDB changes
// in place -- the retention, and the consumers, added, changed and dropped --
// is changed by ALTER TOPIC, and what it does not -- an option of the
// changefeed, and a consumer's codecs taken away -- by dropping and adding it
// again, with a note that says what the restart costs. Each step reads back as
// declared and ends with nothing left to plan, and the removal notes the
// consumers it loses. The statements each step runs are pinned by
// TestStatements_HappyPath in internal/changefeeds.
func TestHostChangesChangefeedsInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostChangefeeds(c)
	eventsTable := h.directory + "/events"
	feed := func(mode, retention, consumers string) string {
		text := "    changefeeds:\n      feed:\n        mode: " + mode + "\n        format: JSON\n"
		if retention != "" {
			text += "        retention_period: " + retention + "\n"
		}
		return text + "        consumers:\n" + consumers
	}
	steps := []struct {
		name, text, note string
		want             ydbdecl.Changefeed
	}{
		{name: "the changefeed added", text: feed("UPDATES", "PT6H", "          a: {supported_codecs: raw}\n          b: {}\n"),
			want: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "PT6H", Consumers: []ydbdecl.Consumer{{Name: "a", SupportedCodecs: []string{"raw"}}, {Name: "b"}}}},
		{name: "the retention set back, a consumer changed, one dropped and one added",
			text: feed("UPDATES", "", "          a: {important: true, supported_codecs: \"raw,zstd\"}\n          c: {read_from: \"2026-01-01T00:00:00Z\"}\n"),
			want: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON",
				Consumers: []ydbdecl.Consumer{{Name: "a", Important: true, SupportedCodecs: []string{"raw", "zstd"}}, {Name: "c", ReadFrom: "2026-01-01T00:00:00Z"}}}},
		{name: "a consumer's codecs taken away", text: feed("UPDATES", "", "          a: {important: true}\n          c: {read_from: \"2026-01-01T00:00:00Z\"}\n"),
			note: "Consumer a of changefeed feed of table " + eventsTable + " is dropped and added again, because YDB keeps a consumer's codecs once it has any.",
			want: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON", Consumers: []ydbdecl.Consumer{{Name: "a", Important: true}, {Name: "c", ReadFrom: "2026-01-01T00:00:00Z"}}}},
		{name: "the mode changed", text: feed("KEYS_ONLY", "", "          a: {important: true}\n          c: {read_from: \"2026-01-01T00:00:00Z\"}\n"),
			note: "Changefeed feed of table " + eventsTable + " is dropped and added again, because YDB changes no option of a changefeed in place. Its stream restarts: " +
				"the records nobody read are lost, and consumers a, c lose their position and start again from the beginning of the new stream.",
			want: ydbdecl.Changefeed{Name: "feed", Mode: "KEYS_ONLY", Format: "JSON", Consumers: []ydbdecl.Consumer{{Name: "a", Important: true}, {Name: "c", ReadFrom: "2026-01-01T00:00:00Z"}}}},
	}
	h.apply("the table", h.events(""), "Schema apply completed successfully")

	for _, step := range steps {
		output := h.apply(step.name, h.events(step.text), "Schema apply completed successfully")
		c.Assert(output, qt.Contains, step.note, qt.Commentf("step: %s", step.name))
		h.apply(step.name+", run again", h.events(step.text), "Schema is synced, no changes to be made")
		c.Assert(h.read(), qt.DeepEquals, []ydbdecl.Changefeed{step.want}, qt.Commentf("step: %s", step.name))
	}

	removed := h.apply("the removal", h.events(""), "Schema apply completed successfully")
	c.Assert(removed, qt.Contains, "Changefeed feed of table "+eventsTable+" is dropped with its topic: the records nobody read are lost, and so are consumers a, c.")
	c.Assert(h.read(), qt.HasLen, 0)
}

// Ported from the host's TestYDBChangefeeds_LeavesWhatItDoesNotModel: a
// changefeed that holds attributes, which the table service takes and YQL has
// no spelling for, is read as one the provider cannot represent. A schema that
// leaves it out and one that declares a changefeed of its name each plan
// nothing and report the comparison they could not make, and the changefeed
// stays as it was. The host's engine refuses the comparison as incomplete;
// the host reports a provider's undecided comparison as a limit and exits 0.
func TestHostKeepsAChangefeedItCannotModel_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostChangefeeds(c)
	h.apply("the table", h.events(""), "Schema apply completed successfully")
	client := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(h.opened.Driver))
	opened, err := client.CreateSession(c.Context(), &Ydb_Table.CreateSessionRequest{})
	c.Assert(err, qt.IsNil)
	var created Ydb_Table.CreateSessionResult
	c.Assert(opened.GetOperation().GetResult().UnmarshalTo(&created), qt.IsNil)
	c.Cleanup(func() {
		_, _ = client.DeleteSession(context.Background(), &Ydb_Table.DeleteSessionRequest{SessionId: created.GetSessionId()})
	})
	altered, err := client.AlterTable(c.Context(), &Ydb_Table.AlterTableRequest{SessionId: created.GetSessionId(), Path: path.Join(h.database, h.directory, "events"),
		AddChangefeeds: []*Ydb_Table.Changefeed{{Name: "tagged", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON,
			Attributes: map[string]string{"owner": "team"}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(altered.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	const limit = `changefeed "tagged" has settings this provider cannot represent, so the current changefeed was not established: it holds attributes, which Ptah does not model.`

	omitted := h.apply("a schema without it", h.events(""), "No changes planned, but 1 comparison limit remains unresolved.")
	named := h.apply("a schema naming it", h.events("    changefeeds:\n      tagged:\n        mode: UPDATES\n        format: JSON\n"),
		"No changes planned, but 1 comparison limit remains unresolved.")

	c.Assert(omitted, qt.Contains, limit)
	c.Assert(named, qt.Contains, limit)
	shapes, found := describedChangefeeds(c, h.opened, path.Join(h.database, h.directory, "events"))
	c.Assert(found, qt.IsTrue)
	c.Assert(shapes, qt.DeepEquals, map[string]feedShape{"tagged": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour}})
}
