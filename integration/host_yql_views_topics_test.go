//go:build integration

package integration_test

import (
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBDesiredYQL_ViewsAndTopics: YQL declares a
// table and a coordination node, beside a view and a topic that each run
// changes. The view's query changes, the topic's consumer is swapped for one
// that reads from a point in time and its retention changes, and then the
// view and the topic are left out and dropped. Each run leaves nothing to
// plan, and the node stays as it was. The host's test runs in a dev realm;
// here the schema names the fixture directory.
func TestHostAppliesYQLViewsAndTopics_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	replacer := strings.NewReplacer("{dir}", directory)
	table := "CREATE TABLE `{dir}/items` (id Int64 NOT NULL, PRIMARY KEY (id)); CREATE COORDINATION NODE `{dir}/locks`;"
	var nodes []nodeSettings
	for _, step := range []struct {
		name, source, view string
		topic              topicShape
		held               []string
	}{
		{name: "created", source: table + " CREATE VIEW `{dir}/summary` WITH (security_invoker = TRUE) AS SELECT id FROM `{dir}/items`; " +
			"CREATE TOPIC `{dir}/events` (CONSUMER worker WITH (important = TRUE)) WITH (retention_period = Interval('P1D'));",
			view: "SELECT id FROM `{dir}/items`", topic: topicShape{MinActivePartitions: 1, Retention: 24 * time.Hour, Consumers: []string{"worker important"}},
			held: []string{"events", "items", "locks", "summary"}},
		{name: "changed", source: table + " CREATE VIEW `{dir}/summary` WITH (security_invoker = TRUE) AS SELECT id FROM `{dir}/items` WHERE id > 0; " +
			"CREATE TOPIC `{dir}/events` (CONSUMER audit WITH (read_from = Timestamp('2026-01-01T00:00:00Z'))) WITH (retention_period = Interval('P2D'));",
			view: "SELECT id FROM `{dir}/items` WHERE id > 0", topic: topicShape{MinActivePartitions: 1, Retention: 48 * time.Hour, Consumers: []string{"audit"}},
			held: []string{"events", "items", "locks", "summary"}},
		{name: "left out", source: table, held: []string{"items", "locks"}},
	} {
		applyRuns(c, directory, "sql", replacer.Replace(step.source))
		c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, step.held, qt.Commentf("%s", step.name))
		topic, _ := describedTopic(c, opened, database, directory, "events")
		c.Assert(topic, qt.DeepEquals, step.topic, qt.Commentf("%s", step.name))
		c.Assert(heldViewQuery(c, opened, database, directory, "summary"), qt.Equals, replacer.Replace(step.view), qt.Commentf("%s", step.name))
		node, found := describedNode(c, opened, database, directory, "locks")
		c.Assert(found, qt.IsTrue, qt.Commentf("%s", step.name))
		nodes = append(nodes, node)
	}
	c.Assert(nodes[1:], qt.DeepEquals, []nodeSettings{nodes[0], nodes[0]})
}

// heldViewQuery is the query the server keeps for the view name in
// directory, or "" when the directory holds no entry of that name.
func heldViewQuery(c *qt.C, opened *connection.Connection, database, directory, name string) string {
	c.Helper()
	if !slices.Contains(listedNames(c, opened, path.Join(database, directory)), name) {
		return ""
	}
	return storedView(c, opened, path.Join(database, directory, name))
}
