//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// selectionRow is one selection of a scoped diff over the directories in
// schemas and the statements it plans.
type selectionRow struct {
	name, schemas string
	flags, want   []string
}

// A table's changefeeds go with the table on both sides of a scoped
// comparison: an include of the table, an exclusion of the other table and a
// --schemas of the table's directory each plan the changefeed the
// declaration adds to the table, and neither the changefeed the declaration
// adds to the other table nor a change of the changefeed the table holds.
// Had a side lost the held changefeed, the plan would add it again or drop
// it. The statements are the built-in engine's at 74de0b65b on 25.1.4.7 and
// 26.2.1.14. Ported from the host's TestScopeDatabasePreservesSelectedFeatureChildren
// and TestScopeGeneratedPreservesSelectedFeatureChildren (internal/atlasfilter);
// their coverage half has no counterpart here, since no changefeed a client
// creates reads as unrepresentable or uninspected.
func TestHostScopesChangefeedsWithTheirTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, app, database := inspectionFixture(c)
	other := app + "_other"
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(database, other)), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Scheme().RemoveDirectory(ctx, path.Join(database, other)), qt.IsNil)
	})
	emptyAtCleanup(c, opened, path.Join(database, other))
	emptyAtCleanup(c, opened, path.Join(database, app))
	for _, statement := range []string{
		"CREATE TABLE `" + path.Join(database, app, "kept") + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE `" + path.Join(database, other, "dropped") + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"ALTER TABLE `" + path.Join(database, app, "kept") + "` ADD CHANGEFEED `updates` WITH (MODE = 'NEW_IMAGE', FORMAT = 'JSON')",
		"ALTER TABLE `" + path.Join(database, other, "dropped") + "` ADD CHANGEFEED `updates` WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	table := func(schema, name, mode string) string {
		return "  " + name + ":\n    schema: " + schema + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n" +
			"    changefeeds:\n      updates:\n        mode: " + mode + "\n        format: JSON\n      audit:\n        mode: UPDATES\n        format: JSON\n"
	}
	declared := "tables:\n" + table(app, "kept", "NEW_IMAGE") + table(other, "dropped", "KEYS_ONLY")
	audit := func(schema, name string) string {
		return "ALTER TABLE `" + schema + "/" + name + "` ADD CHANGEFEED `audit` WITH (MODE = 'UPDATES', FORMAT = 'JSON')"
	}
	both := app + "," + other

	for _, row := range []selectionRow{
		{name: "include the table", schemas: both, flags: []string{"--include", app + ".kept"}, want: []string{audit(app, "kept")}},
		{name: "exclude the other table", schemas: both, flags: []string{"--exclude", other + ".dropped"}, want: []string{audit(app, "kept")}},
		{name: "the table's directory", schemas: app, want: []string{audit(app, "kept")}},
		{name: "both directories", schemas: both, want: []string{audit(app, "kept"), audit(other, "dropped")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(scopedStatements(c, row.schemas, declared, row.flags...), qt.DeepEquals, row.want)
		})
	}
}

// A streaming query selected by name keeps the resource pool it runs in, the
// pool default when it names none, on both sides of a scoped comparison,
// and an exclusion of a query keeps every pool. Each pool's change is
// planned alone: altering the pool a query runs in does not restart the
// query, since YDB alters the pool beside the running query and the query
// keeps running. The statements are the built-in engine's at 74de0b65b on
// 26.2.1.14. This provider restarted the query around its pool's change,
// stopping it for nothing, and refused the include of the query, whose
// scope keeps the pool but not the topics a restart needs (G235); it kept no
// pool default for a query that names none (G236). Ported from the host's
// TestScope_StreamingQueriesAndTheirPools (internal/atlasfilter); its
// coverage half, TestScopeStreamingLimitsFollowObjectSelection, has no
// counterpart here, since no query a client creates reads as unrepresentable
// or uninspected.
func TestHostScopesStreamingQueriesWithTheirPools_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	_, session := accessSession(c)
	skipWithout(c, session, streaming.Capability)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	batch, idle := directory+"_batch", directory+"_idle"
	createSchemaObject(c, opened, "CREATE RESOURCE POOL `"+batch+"` WITH (CONCURRENT_QUERY_LIMIT = 2)", "DROP RESOURCE POOL `"+batch+"`")
	createSchemaObject(c, opened, "CREATE RESOURCE POOL `"+idle+"` WITH (CONCURRENT_QUERY_LIMIT = 3)", "DROP RESOURCE POOL `"+idle+"`")
	body := "INSERT INTO `" + path.Join(database, directory, "sink") + "` SELECT * FROM `" + path.Join(database, directory, "source") + "`;"
	for _, name := range []string{"source", "sink"} {
		createSchemaObject(c, opened, "CREATE TOPIC `"+path.Join(database, directory, name)+"`", "DROP TOPIC `"+path.Join(database, directory, name)+"`")
	}
	createSchemaObject(c, opened, streaming.Create(directory, "copy", streaming.Spec{Text: body, ResourcePool: batch}),
		"DROP STREAMING QUERY `"+path.Join(directory, "copy")+"`")
	createSchemaObject(c, opened, streaming.Create(directory, "other", streaming.Spec{Text: body}), "DROP STREAMING QUERY `"+path.Join(directory, "other")+"`")
	declared := "topics:\n  source:\n    schema: " + directory + "\n  sink:\n    schema: " + directory + "\n" +
		"resource_pools:\n  " + batch + ":\n    concurrent_query_limit: 5\n  " + idle + ":\n    concurrent_query_limit: 7\n" +
		"  default:\n    query_memory_limit_percent_per_node: 50\n" +
		"streaming_queries:\n  copy:\n    schema: " + directory + "\n    resource_pool: " + batch + "\n    text: \"" + body + "\"\n" +
		"  other:\n    schema: " + directory + "\n    text: \"" + body + "\"\n"
	alterDefault := "ALTER RESOURCE POOL `default` SET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 50)"
	alterBatch := "ALTER RESOURCE POOL `" + batch + "` SET (CONCURRENT_QUERY_LIMIT = 5)"
	alterIdle := "ALTER RESOURCE POOL `" + idle + "` SET (CONCURRENT_QUERY_LIMIT = 7)"
	selectors := directory + ",/"

	for _, row := range []selectionRow{
		{name: "include the query in a pool", schemas: selectors, flags: []string{"--include", directory + ".copy[type=streaming_query]"}, want: []string{alterBatch}},
		{name: "include the query in the pool default", schemas: selectors, flags: []string{"--include", directory + ".other[type=streaming_query]"}, want: []string{alterDefault}},
		{name: "exclude a query", schemas: selectors, flags: []string{"--exclude", directory + ".copy[type=streaming_query]"}, want: []string{alterDefault, alterBatch, alterIdle}},
		{name: "everything", schemas: selectors, want: []string{alterDefault, alterBatch, alterIdle}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(scopedStatements(c, row.schemas, declared, row.flags...), qt.DeepEquals, row.want)
		})
	}
}
