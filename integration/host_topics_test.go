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
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
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
			c := qt.New(t)
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
