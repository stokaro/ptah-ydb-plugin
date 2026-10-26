//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// The host plans resource pools and classifiers through this provider from
// each source that declares them: it creates them, finds nothing to change on
// the next run, changes a pool's limits and swaps two classifiers' ranks, and
// keeps them all once the source leaves them out, since a pool and a
// classifier belong to the whole database rather than to the directory a
// source describes.
func TestHostAppliesResourcePools_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format                  string
		created, changed, empty string
	}{
		{format: "yaml",
			created: "resource_pools:\n  {pool}:\n    concurrent_query_limit: 2\n    queue_size: 4\nresource_pool_classifiers:\n" +
				"  {a}:\n    resource_pool: {pool}\n    rank: {low}\n  {b}:\n    resource_pool: default\n    member_name: etl\n    rank: {high}\n",
			changed: "resource_pools:\n  {pool}:\n    concurrent_query_limit: 5\nresource_pool_classifiers:\n" +
				"  {a}:\n    resource_pool: {pool}\n    rank: {high}\n  {b}:\n    resource_pool: {pool}\n    rank: {low}\n",
			empty: "coordination_nodes: {}\n"},
		{format: "go",
			created: "package entities\n\n" + goItems + "\n//ptah:schema:resourcepool name=\"{pool}\" concurrent_query_limit=\"2\" queue_size=\"4\"\n" +
				"//ptah:schema:resourcepool:classifier name=\"{a}\" resource_pool=\"{pool}\" rank=\"{low}\"\n" +
				"//ptah:schema:resourcepool:classifier name=\"{b}\" resource_pool=\"default\" member_name=\"etl\" rank=\"{high}\"\ntype Pools struct{}\n",
			changed: "package entities\n\n" + goItems + "\n//ptah:schema:resourcepool name=\"{pool}\" concurrent_query_limit=\"5\"\n" +
				"//ptah:schema:resourcepool:classifier name=\"{a}\" resource_pool=\"{pool}\" rank=\"{high}\"\n" +
				"//ptah:schema:resourcepool:classifier name=\"{b}\" resource_pool=\"{pool}\" rank=\"{low}\"\ntype Pools struct{}\n",
			empty: "package entities\n\n" + goItems},
		{format: "sql",
			created: sqlItems + "CREATE RESOURCE POOL `{pool}` WITH (CONCURRENT_QUERY_LIMIT = 2, QUEUE_SIZE = 4);\n" +
				"CREATE RESOURCE POOL CLASSIFIER `{a}` WITH (RESOURCE_POOL = '{pool}', RANK = {low});\n" +
				"CREATE RESOURCE POOL CLASSIFIER `{b}` WITH (RESOURCE_POOL = 'default', MEMBER_NAME = 'etl', RANK = {high});\n",
			changed: sqlItems + "CREATE RESOURCE POOL `{pool}` WITH (CONCURRENT_QUERY_LIMIT = 5);\n" +
				"CREATE RESOURCE POOL CLASSIFIER `{a}` WITH (RESOURCE_POOL = '{pool}', RANK = {high});\n" +
				"CREATE RESOURCE POOL CLASSIFIER `{b}` WITH (RESOURCE_POOL = '{pool}', RANK = {low});\n",
			empty: sqlItems},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			enableResourcePools(c, opened)
			poolName, first, second := directory+"_pool", directory+"_a", directory+"_b"
			low, high := workloadRanks(directory)
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				for _, name := range []string{first, second} {
					_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL CLASSIFIER `"+name+"`")
				}
				_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL `"+poolName+"`")
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+database+"/"+directory+"/items`"), qt.IsNil)
			})
			replacer := strings.NewReplacer("{dir}", directory, "{pool}", poolName, "{a}", first, "{b}", second,
				"{low}", strconv.FormatInt(low, 10), "{high}", strconv.FormatInt(high, 10))
			apply := func(name, text, want string) {
				c.Helper()
				// A pool and a classifier take the root's namespace, so the
				// run selects it beside the fixture directory.
				output, err := hostApplySelected(c, directory+",/", test.format, replacer.Replace(text))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
			}
			held := func() (map[string]heldPool, map[string]heldClassifier) {
				c.Helper()
				return readHeldWorkload(c, c.Context(), opened.DB, database, []string{poolName}, []string{first, second})
			}
			apply("creation", test.created, `(?s).*Schema apply completed successfully.*`)
			pools, classifiers := held()
			c.Assert(pools, qt.DeepEquals, map[string]heldPool{poolName: {ConcurrentQueryLimit: 2, QueueSize: 4, ResourceWeight: -1}})
			c.Assert(classifiers, qt.DeepEquals, map[string]heldClassifier{first: {Pool: poolName, Rank: low}, second: {Pool: "default", Member: "etl", Rank: high}})
			apply("a second run", test.created, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a change", test.changed, `(?s).*Schema apply completed successfully.*`)
			pools, classifiers = held()
			c.Assert(pools, qt.DeepEquals, map[string]heldPool{poolName: {ConcurrentQueryLimit: 5, QueueSize: -1, ResourceWeight: -1}})
			c.Assert(classifiers, qt.DeepEquals, map[string]heldClassifier{first: {Pool: poolName, Rank: high}, second: {Pool: poolName, Rank: low}})
			apply("a run after the change", test.changed, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a source that leaves them out", test.empty, `(?s).*Schema apply completed successfully.*|(?s).*Schema is synced, no changes to be made.*`)
			pools, classifiers = held()
			c.Assert(pools, qt.HasLen, 1)
			c.Assert(classifiers, qt.HasLen, 2)
		})
	}
}
