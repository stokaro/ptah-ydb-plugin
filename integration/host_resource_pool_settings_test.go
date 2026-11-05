//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// heldPoolSettings is every setting .sys/resource_pools reports of a pool
// that the host's round trip declares, -1 for none.
type heldPoolSettings struct {
	ConcurrentQueryLimit, QueueSize                float64
	QueryMemoryLimitPercentPerNode, ResourceWeight float64
}

func readPoolSettings(c *qt.C, db *sql.DB, database, name string) heldPoolSettings {
	c.Helper()
	var held heldPoolSettings
	c.Assert(db.QueryRowContext(c.Context(), "SELECT CAST(ConcurrentQueryLimit AS Double), CAST(QueueSize AS Double), QueryMemoryLimitPercentPerNode, ResourceWeight FROM `"+
		path.Join(database, ".sys/resource_pools")+"` WHERE Name = $name", sql.Named("name", name)).Scan(
		&held.ConcurrentQueryLimit, &held.QueueSize, &held.QueryMemoryLimitPercentPerNode, &held.ResourceWeight), qt.IsNil)
	return held
}

// roundTripPools declares, in YAML, pool batch with the settings given, pool
// idle with none, the pool default with a weight, and two classifiers that
// send a member no user holds to the two pools at the ranks given.
func roundTripPools(batch string, batchRank, idleRank int64) string {
	return "resource_pools:\n  {batch}:\n" + batch + "  {idle}: {}\n  default:\n    resource_weight: 30\n" +
		"resource_pool_classifiers:\n  {to_batch}:\n    resource_pool: {batch}\n    member_name: {member}\n    rank: " + strconv.FormatInt(batchRank, 10) + "\n" +
		"  {to_idle}:\n    resource_pool: {idle}\n    member_name: {member}g\n    rank: " + strconv.FormatInt(idleRank, 10) + "\n"
}

// Ported from the host's TestYDBResourcePools_RoundTrip, for what
// TestHostAppliesResourcePools_HappyPath leaves out: a pool with a fractional
// memory share, a pool that states no setting and so has no limit, and the
// pool default with a weight. The host creates them with two classifiers and
// finds nothing to change on the next two runs. A changed limit with the
// queue and the memory share left out, and two classifiers that trade ranks,
// which YDB refuses step by step, are applied in one run and leave nothing to
// plan. The weight given to the pool default is reset when the test ends.
func TestHostAppliesResourcePoolSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	names := map[string]string{"batch": directory + "_batch", "idle": directory + "_idle", "to_batch": directory + "_cb", "to_idle": directory + "_ci",
		"member": directory + "_user"}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP RESOURCE POOL CLASSIFIER `" + names["to_batch"] + "`", "DROP RESOURCE POOL CLASSIFIER `" + names["to_idle"] + "`",
			"DROP RESOURCE POOL `" + names["batch"] + "`", "DROP RESOURCE POOL `" + names["idle"] + "`", "ALTER RESOURCE POOL default RESET (RESOURCE_WEIGHT)"} {
			_ = execFixtureSchema(ctx, opened.DB, statement)
		}
	})
	replacer := strings.NewReplacer("{batch}", names["batch"], "{idle}", names["idle"], "{to_batch}", names["to_batch"], "{to_idle}", names["to_idle"],
		"{member}", names["member"])
	low, high := workloadRanks(directory)
	selectors := directory + ",/"

	applyUntilSynced(c, selectors, "yaml", replacer.Replace(roundTripPools("    concurrent_query_limit: 5\n    queue_size: 4\n    query_memory_limit_percent_per_node: 12.5\n", low, high)))
	c.Assert(readPoolSettings(c, opened.DB, database, names["batch"]), qt.Equals, heldPoolSettings{5, 4, 12.5, -1})
	c.Assert(readPoolSettings(c, opened.DB, database, names["idle"]), qt.Equals, heldPoolSettings{-1, -1, -1, -1})
	c.Assert(readPoolSettings(c, opened.DB, database, "default").ResourceWeight, qt.Equals, float64(30))
	_, classifiers := readHeldWorkload(c, c.Context(), opened.DB, database, nil, []string{names["to_batch"], names["to_idle"]})
	c.Assert(classifiers, qt.DeepEquals, map[string]heldClassifier{names["to_batch"]: {Pool: names["batch"], Member: names["member"], Rank: low},
		names["to_idle"]: {Pool: names["idle"], Member: names["member"] + "g", Rank: high}})

	applyRuns(c, selectors, "yaml", replacer.Replace(roundTripPools("    concurrent_query_limit: 7\n", high, low)))

	c.Assert(readPoolSettings(c, opened.DB, database, names["batch"]), qt.Equals, heldPoolSettings{7, -1, -1, -1})
	c.Assert(readPoolSettings(c, opened.DB, database, names["idle"]), qt.Equals, heldPoolSettings{-1, -1, -1, -1})
	c.Assert(readPoolSettings(c, opened.DB, database, "default").ResourceWeight, qt.Equals, float64(30))
	_, classifiers = readHeldWorkload(c, c.Context(), opened.DB, database, nil, []string{names["to_batch"], names["to_idle"]})
	c.Assert(classifiers, qt.DeepEquals, map[string]heldClassifier{names["to_batch"]: {Pool: names["batch"], Member: names["member"], Rank: high},
		names["to_idle"]: {Pool: names["idle"], Member: names["member"] + "g", Rank: low}})
}
