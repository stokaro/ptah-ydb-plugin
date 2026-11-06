//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// workloadOrderSource declares a group, a table t, a topic events, a pool
// {pool}, a classifier {classifier} routing {member} to it, and a grant to the
// group on the pool, on t and on events, in the directory {dir}.
const workloadOrderSource = `roles:
  {group}:
    group: true
tables:
  t:
    schema: {dir}
    columns:
      id: {type: Uint64, primary: true}
topics:
  events:
    schema: {dir}
resource_pools:
  {pool}:
    concurrent_query_limit: 10
resource_pool_classifiers:
  {classifier}:
    resource_pool: {pool}
    member_name: {member}
    rank: {rank}
grants:
  grant_t:
    role: {group}
    privileges: [SELECT ROW]
    on_table: {dir}.t
  grant_events:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_topic: {dir}.events
  grant_pool:
    role: {group}
    privileges: [SELECT ROW]
    on_resource_pool: {pool}
`

// `schema apply` and `migrations generate` create a resource pool right before
// the grant on it and its classifier after every grant, as the YDB engine
// built into the host at 74de0b65b did on 25.1.4.7 and 26.2.1.14: the topic,
// the group, the table, the pool, the grants on the pool, on t and on events,
// and then the classifier. The down file drops the topic, the table, the
// classifier, the pool and then the group, as the built-in's did. The provider
// planned the pool and the classifier in one step ahead of the grants, and
// dropped both ahead of the table.
func TestHostCreatesAClassifierAfterTheGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, _ := lockFixture(c)
	enableResourcePools(c, opened)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	group, pool, classifier := principalName("ptahwork"), directory+"_pool", directory+"_c"
	rank, _ := workloadRanks(directory)
	source := strings.NewReplacer("{dir}", directory, "{group}", group, "{pool}", pool, "{classifier}", classifier,
		"{member}", workloadMember(directory), "{rank}", strconv.FormatInt(rank, 10)).Replace(workloadOrderSource)
	file := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	migrations := c.TempDir()
	// A pool and a classifier take the root's namespace, so the runs select
	// it beside the fixture directory.
	selectors := directory + ",/"
	at := func(object string) string { return "`" + object + "` TO `" + group + "`" }

	units := hostPlannedUnits(c, selectors, "yaml", source)
	var planned, workload []string
	for _, unit := range units {
		planned = append(planned, unit.head())
	}
	// The host names a workload unit by its kind and the database's
	// workload, so the reason each one's assessment gives tells the pool's
	// from the classifier's.
	for _, unit := range slices.DeleteFunc(slices.Clone(units), func(unit plannedUnit) bool { return !strings.HasSuffix(unit.Native.Kind, "/workload-operation") }) {
		workload = append(workload, unit.Native.Reason)
	}
	generated, generateErr := hostCommand(c, "migrations", "generate", "--migrations-dir", migrations, "--name", "order",
		"--db-url", address, "--schemas", selectors, "--schema-file", file)
	up, down := generatedPair(c, migrations)

	c.Assert(planned, qt.DeepEquals, []string{
		"topic-operation " + directory + ".events",
		"CREATE GROUP `" + group + "`",
		"row-table-create-operation " + directory + ".t",
		"workload-operation workload",
		"GRANT 'ydb.granular.select_row' ON " + at(".metadata/workload_manager/pools/"+pool),
		"GRANT 'ydb.granular.select_row' ON " + at(directory+"/t"),
		"GRANT 'ydb.granular.describe_schema' ON " + at(directory+"/events"),
		"workload-operation workload",
	})
	c.Assert(workload, qt.DeepEquals, []string{
		"CREATE RESOURCE POOL adds a resource pool; it does not remove data or tighten constraints",
		"resource pool classifier settings change which pool receives matching queries",
	})
	c.Assert(generateErr, qt.IsNil, qt.Commentf("%s", generated))
	c.Assert(renderedStatements(up), qt.DeepEquals, []string{
		"CREATE TOPIC " + directory + "/events", "CREATE GROUP " + group, "CREATE TABLE " + directory + "/t", "CREATE RESOURCE POOL " + pool,
		"GRANT ON .metadata/workload_manager/pools/" + pool, "GRANT ON " + directory + "/t", "GRANT ON " + directory + "/events",
		"CREATE RESOURCE POOL CLASSIFIER " + classifier,
	}, qt.Commentf("%s", up))
	c.Assert(renderedStatements(down), qt.DeepEquals, []string{
		"DROP TOPIC " + directory + "/events", "DROP TABLE " + directory + "/t", "DROP RESOURCE POOL CLASSIFIER " + classifier,
		"DROP RESOURCE POOL " + pool, "DROP GROUP IF EXISTS " + group,
	}, qt.Commentf("%s", down))
}
