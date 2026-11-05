//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// contextTopics declares, in the fixture directory, the topic events with
// the retention given, and the table items when withTable is set.
func contextTopics(directory, retention string, withTable bool) string {
	text := "topics:\n  events:\n    schema: " + directory + "\n    retention_period: " + retention + "\n"
	if withTable {
		text += "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n      ts: {type: Timestamp}\n"
	}
	return text
}

// outsideReader is a fixture directory whose topic events a streaming query
// in a second directory reads, on a line with streaming queries.
type outsideReader struct {
	opened              *connection.Connection
	database, directory string
	outer               string
}

// newOutsideReader locks a fixture directory and names a second directory
// beside it for the reading query. The cleanup empties the second directory
// first, so the query goes before the topic it reads.
func newOutsideReader(c *qt.C) *outsideReader {
	c.Helper()
	opened, directory, database := lockFixture(c)
	_, session := accessSession(c)
	skipWithout(c, session, streaming.Capability)
	f := &outsideReader{opened: opened, database: database, directory: directory, outer: directory + "_reader"}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(database, f.outer))
	})
	return f
}

// both is the scope of the two directories.
func (f *outsideReader) both() string { return f.directory + "," + f.outer }

// declared is the whole schema of the two directories: the topic events with
// the retention given, the topic sink, and the query copy between them,
// running or stopped.
func (f *outsideReader) declared(retention, run string) string {
	return contextTopics(f.directory, retention, false) + "  sink:\n    schema: " + f.outer + "\n" +
		"streaming_queries:\n  copy:\n    schema: " + f.outer + "\n    text: \"INSERT INTO `" + f.outer + "/sink` SELECT * FROM `" + f.directory + "/events`;\"\n    run: " + run + "\n"
}

// retention reads the topic events back.
func (f *outsideReader) retention(c *qt.C) time.Duration {
	c.Helper()
	topic, found := describedTopic(c, f.opened, f.database, f.directory, "events")
	c.Assert(found, qt.IsTrue)
	return topic.Retention
}

// running reports whether the reading query runs.
func (f *outsideReader) running(c *qt.C) bool {
	c.Helper()
	spec, err := nativeapi.ReadStreamingQuery(c.Context(), f.opened.DB, f.database, path.Join(f.database, f.outer, "copy"))
	c.Assert(err, qt.IsNil)
	c.Assert(spec, qt.IsNotNil)
	return streaming.Running(*spec)
}

// skipWithoutContextPerService skips on a host that gives a planning service
// the context records of another service's kinds (G115, stokaro/ptah#5312),
// which the SDK refuses before the provider plans anything. It skips on that
// refusal alone, so any other answer still fails the test.
func skipWithoutContextPerService(c *qt.C, output string) {
	c.Helper()
	if strings.Contains(output, "planning context inventory: a record is of a kind the service does not assess") {
		c.Skip("the selected host sends a planning service context records of another service's kinds (stokaro/ptah#5312)")
	}
}

// Measured for stokaro/ptah#5169: a read filtered with --schemas carries the
// streaming query inventory a plan needs (ADR 0054). In a database where no
// query reads the fixture's topic, a filtered run changes the topic's
// retention and drops a table, and each next run finds nothing to change.
func TestHostChangesAnInputUnderAFilteredRead_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	applyRuns(c, directory, "yaml", contextTopics(directory, "PT24H", true))
	applyRuns(c, directory, "yaml", contextTopics(directory, "PT48H", true))
	topic, found := describedTopic(c, opened, database, directory, "events")
	c.Assert(found, qt.IsTrue)
	c.Assert(topic.Retention, qt.Equals, 48*time.Hour)
	applyRuns(c, directory, "yaml", contextTopics(directory, "PT48H", false))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"events"})
}

// A streaming query outside the filtered scope reads the changed topic. The
// plan stops a query only when the scope selects it, so with both directories
// in scope it stops the query, changes the topic and starts the query again;
// a stopped query outside the scope needs no stop, and the filtered change
// applies. The built-in engine changes the topic under a running query
// without stopping it.
func TestHostChangesAnInputAQueryOutsideTheScopeReads_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newOutsideReader(c)
	applyRuns(c, f.both(), "yaml", f.declared("PT24H", "true"))
	c.Assert(hostPlanned(c, f.both(), "yaml", f.declared("PT48H", "true")), qt.DeepEquals, []string{
		"streaming-query-operation " + f.outer + ".copy", "topic-operation " + f.directory + ".events", "streaming-query-operation " + f.outer + ".copy",
	})
	applyRuns(c, f.both(), "yaml", f.declared("PT48H", "true"))
	c.Assert([]any{f.retention(c), f.running(c)}, qt.DeepEquals, []any{48 * time.Hour, true})

	applyRuns(c, f.both(), "yaml", f.declared("PT48H", "false"))
	output, err := hostApplySelected(c, f.directory, "yaml", contextTopics(f.directory, "PT72H", false))
	skipWithoutContextPerService(c, output)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	c.Assert(hostPlanned(c, f.directory, "yaml", contextTopics(f.directory, "PT72H", false)), qt.HasLen, 0)
	c.Assert([]any{f.retention(c), f.running(c)}, qt.DeepEquals, []any{72 * time.Hour, false})
}

// A running streaming query outside the filtered scope reads the topic. The
// plan cannot stop a query the scope leaves out, so a filtered change of the
// topic and a filtered drop of it are refused, naming the query with the
// directory to include, and both change nothing. The built-in engine drops
// the topic and leaves the query reading nothing; we do not copy that.
func TestHostRefusesAnInputChangeAQueryOutsideTheScopeReads_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newOutsideReader(c)
	applyRuns(c, f.both(), "yaml", f.declared("PT24H", "true"))
	refusal := `streaming query "` + f.outer + `/copy" outside the command's scope reads a changed input; include its directory in the scope so the plan can stop and restart it`
	for _, test := range []struct{ name, source string }{
		{"a changed retention", contextTopics(f.directory, "PT48H", false)},
		{"a dropped topic", "topics: {}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			output, err := hostApplySelected(c, f.directory, "yaml", test.source)
			skipWithoutContextPerService(c, output)
			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, refusal)
			c.Assert([]any{f.retention(c), f.running(c)}, qt.DeepEquals, []any{24 * time.Hour, true})
		})
	}
}
