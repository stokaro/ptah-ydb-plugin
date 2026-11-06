//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// poolGrantSources are the sources a row of
// TestHostKeepsTheGrantOnAPoolTheSourceLeavesOut_HappyPath plans, by name:
// each declares the group {group}, the table {dir}/t and the group's grant on
// the table, in YAML or YQL, and either leaves the resource pools out or
// declares the pool {other} alone.
var poolGrantSources = map[string]struct{ format, text string }{
	"yaml": {"yaml", "roles:\n  {group}:\n    group: true\ntables:\n  t:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true}\n" +
		"grants:\n  grant_t:\n    role: {group}\n    privileges: [SELECT ROW]\n    on_table: {dir}.t\n"},
	"sql": {"sql", "CREATE GROUP `{group}`;\nCREATE TABLE `{dir}/t` (`id` Uint64 NOT NULL, PRIMARY KEY (`id`));\n" +
		"GRANT 'ydb.granular.select_row' ON `{dir}/t` TO `{group}`;\n"},
	"another pool": {"yaml", "roles:\n  {group}:\n    group: true\ntables:\n  t:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true}\n" +
		"resource_pools:\n  {other}:\n    concurrent_query_limit: 3\n" +
		"grants:\n  grant_t:\n    role: {group}\n    privileges: [SELECT ROW]\n    on_table: {dir}.t\n"},
}

// A resource pool belongs to the whole database, so a source that leaves it
// out keeps it, and the YDB engine built into the host at 74de0b65b kept the
// grants on it too, on 25.1.4.7 and 26.2.1.14: a source that declares a group,
// a table and the group's grant on the table, and no pool or another pool,
// plans nothing for the group's grant on a pool the database holds, and an
// apply of it leaves the grant where it was. A source that declares the pool
// manages its grants, so leaving the grant out revokes it. Without the host's
// rule for an object its owner keeps, every one of these sources planned a
// REVOKE on the pool, and an apply took the group's permission away.
func TestHostKeepsTheGrantOnAPoolTheSourceLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	group, pool, other := principalName("ptahpool"), directory+"_held", directory+"_other"
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+database+"/"+directory+"/t`"), qt.IsNil)
		_ = execFixtureSchema(ctx, opened.DB, "DROP RESOURCE POOL `"+pool+"`")
		_ = execFixtureSchema(ctx, opened.DB, "DROP RESOURCE POOL `"+other+"`")
	})
	poolPath := ".metadata/workload_manager/pools/" + pool
	for _, statement := range []string{
		"CREATE GROUP `" + group + "`",
		"CREATE TABLE `" + directory + "/t` (`id` Uint64 NOT NULL, PRIMARY KEY (`id`))",
		"CREATE RESOURCE POOL `" + pool + "` WITH (CONCURRENT_QUERY_LIMIT = 10)",
		"GRANT 'ydb.granular.select_row' ON `" + directory + "/t` TO `" + group + "`",
		"GRANT 'ydb.granular.select_row' ON `" + poolPath + "` TO `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	replacer := strings.NewReplacer("{dir}", directory, "{group}", group, "{other}", other)
	// A pool takes the root's namespace, so the runs select it beside the
	// fixture directory.
	selectors := directory + ",/"
	// A session establishes resource pools when it opens, so the read takes
	// one opened after the flag.
	client, session := accessSession(c)
	held := func() []grantEntry {
		c.Helper()
		return grantsInSchemas(c, client, session, []string{directory, "/"}, []string{"resource pool"}, []string{group})
	}
	declared := replacer.Replace(poolGrantSources["yaml"].text) + "resource_pools:\n  " + pool + ":\n    concurrent_query_limit: 10\n"

	plans := make(map[string][]string)
	for name, source := range poolGrantSources {
		plans[name] = hostPlanned(c, selectors, source.format, replacer.Replace(source.text))
	}
	applied, applyErr := hostApplySelected(c, selectors, "yaml", replacer.Replace(poolGrantSources["yaml"].text))
	kept := held()
	managed := hostPlanned(c, selectors, "yaml", declared)

	// The host names a workload unit by its kind and the database's workload.
	c.Assert(plans, qt.DeepEquals, map[string][]string{"yaml": {}, "sql": {}, "another pool": {"workload-operation workload"}})
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema is synced, no changes to be made")
	c.Assert(kept, qt.DeepEquals, []grantEntry{{Role: group, Privilege: "ydb.granular.select_row", Kind: "resource pool", Name: pool}})
	c.Assert(managed, qt.DeepEquals, []string{"REVOKE 'ydb.granular.select_row' ON `" + poolPath + "` FROM `" + group + "`"})
}
