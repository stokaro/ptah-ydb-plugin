//go:build integration

package integration_test

import (
	"context"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// The host's TestYDBDesiredYQL_ChangefeedsAndConsumers. A YQL schema gives a
// table a changefeed with a consumer, swaps the consumer for an important one
// and removes it; each step plans the changefeed operation the built-in YDB
// engine plans for it (ADD CHANGEFEED with ADD CONSUMER, DROP CONSUMER with
// ADD CONSUMER, DROP CONSUMER), applies, plans nothing on the next run, and
// leaves the changefeed's topic with the declared consumers. The schema then
// removes the changefeed, which drops it with its topic. The changefeed
// reaches the host as a facet of its table, which needs a host that lists
// schema.sql-facets.
func TestHostAppliesYQLChangefeedsAndConsumers_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.SQLFacetsFeature)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	topic := path.Join(database, directory, "events", "updates")
	const table = "CREATE TABLE `{dir}/events` (id Int64 NOT NULL, PRIMARY KEY (id));\n"
	const feed = "ALTER TABLE `{dir}/events` ADD CHANGEFEED updates WITH (mode='UPDATES', format='JSON', retention_period=Interval('PT12H'));\n"
	// A changed consumer set is one ALTER TOPIC of the changefeed's topic,
	// which the built-in engine writes as DROP CONSUMER and ADD CONSUMER.
	const alterTopic = "(?s).*changefeed-operation\n[^\n]*: ALTER TOPIC can drop a consumer's position.*Schema apply completed successfully.*"
	apply := func(text, planned, name string) {
		c.Helper()
		root := writeSchema(c, map[string]string{"schema.sql": replaceDirectory(text, directory)})
		schemaFile := filepath.Join(root, "schema.sql")
		output, err := hostApplySQL(c, directory, schemaFile)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
		c.Assert(output, qt.Matches, planned, qt.Commentf("%s", name))
		again, err := hostApplySQL(c, directory, schemaFile)
		c.Assert(err, qt.IsNil, qt.Commentf("%s, again: %s", name, again))
		c.Assert(again, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s", name))
	}
	for _, step := range []struct {
		name, text, planned string
		consumers           []string
	}{
		{"a changefeed with a consumer", table + feed + "ALTER TOPIC `{dir}/events/updates` ADD CONSUMER worker;\n",
			"(?s).*row-table-create-operation.*changefeed-operation\n[^\n]*: ADD CHANGEFEED adds a stream of the table's changes.*Schema apply completed successfully.*",
			[]string{"worker"}},
		{"an important consumer instead", table + feed + "ALTER TOPIC `{dir}/events/updates` ADD CONSUMER audit WITH (important=TRUE);\n",
			alterTopic, []string{"audit (important)"}},
		{"no consumer", table + feed, alterTopic, nil},
	} {
		apply(step.text, step.planned, step.name)
		described, err := opened.Driver.Topic().Describe(c.Context(), topic)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", step.name))
		var consumers []string
		for _, consumer := range described.Consumers {
			consumers = append(consumers, map[bool]string{true: consumer.Name + " (important)", false: consumer.Name}[consumer.Important])
		}
		c.Assert(consumers, qt.DeepEquals, step.consumers, qt.Commentf("%s", step.name))
	}
	apply(table, "(?s).*changefeed-operation\n[^\n]*: DROP CHANGEFEED removes the change stream.*Schema apply completed successfully.*", "no changefeed")
	_, err := opened.Driver.Topic().Describe(c.Context(), topic)
	c.Assert(err, qt.IsNotNil)
}
