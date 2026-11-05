//go:build integration

package integration_test

import (
	"context"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBResourcePools_Rollback: `migrations generate`
// writes the creation of two pools and two classifiers and a change of the
// pool default, and its reverse; `migrations up` applies it and leaves
// nothing to plan, and `migrations down` drops the pools and the classifiers
// and gives the pool default back the settings it held. A pool and a
// classifier take the root's namespace, so the generation selects it beside
// the fixture directory, and the classifiers route members no user holds
// (see workloadMember).
func TestHostRollsBackResourcePools_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	batch, idle, toBatch, toIdle, member := directory+"_batch", directory+"_idle", directory+"_to_batch", directory+"_to_idle", workloadMember(directory)
	low, high := workloadRanks(directory)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{toBatch, toIdle} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL CLASSIFIER `"+name+"`")
		}
		for _, name := range []string{batch, idle} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL `"+name+"`")
		}
		_ = execFixtureSchema(cleanup, opened.DB, "ALTER RESOURCE POOL default RESET (RESOURCE_WEIGHT)")
	})
	pools, classifiers := []string{batch, idle, "default"}, []string{toBatch, toIdle}
	before, _ := readHeldWorkload(c, c.Context(), opened.DB, database, pools, classifiers)
	declared := strings.NewReplacer("{batch}", batch, "{idle}", idle, "{to_batch}", toBatch, "{to_idle}", toIdle, "{member}", member,
		"{low}", strconv.FormatInt(low, 10), "{high}", strconv.FormatInt(high, 10)).Replace(
		"resource_pools:\n  {batch}:\n    concurrent_query_limit: 5\n  {idle}: {}\n  default:\n    resource_weight: 30\n" +
			"resource_pool_classifiers:\n  {to_batch}:\n    resource_pool: {batch}\n    member_name: {member}\n    rank: {low}\n" +
			"  {to_idle}:\n    resource_pool: {idle}\n    member_name: {member}g\n    rank: {high}\n")

	migrations, _, down := hostGenerate(c, directory+",/", "yaml", declared)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward, forwardClassifiers := readHeldWorkload(c, c.Context(), opened.DB, database, pools, classifiers)
	settled := hostPlanned(c, directory+",/", "yaml", declared)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	after, afterClassifiers := readHeldWorkload(c, c.Context(), opened.DB, database, pools, classifiers)

	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.HasLen, 3)
	c.Assert(forward["default"].ResourceWeight, qt.Equals, 30.0)
	c.Assert(forwardClassifiers, qt.HasLen, 2)
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(afterClassifiers, qt.HasLen, 0)
	c.Assert(after, qt.DeepEquals, before)
}
