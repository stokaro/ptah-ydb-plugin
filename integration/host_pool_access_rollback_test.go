//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the rollback half of the host's TestYDBObjectAccess_AResourcePool:
// `migrations generate` writes the creation of a resource pool, a user and
// the user's grant on the pool, and the reverse; `migrations up` applies them
// and the read reports the grant, and `migrations down` drops the pool, whose
// entries go with it, so the down file holds no REVOKE, and the user; the
// declaration then plans what it planned before the generation.
func TestHostRollsBackAccessOnAResourcePool_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	emptyAtCleanup(c, f.opened, path.Join(f.database, f.directory))
	enableResourcePools(c, f.opened)
	// A session establishes resource pools when it opens, so the read takes
	// one opened after the flag.
	client, session := accessSession(c)
	pool := f.directory + "_acl"
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP RESOURCE POOL `"+pool+"`")
	})
	// A pool takes the root's namespace, so the run selects it beside the
	// fixture directory.
	selectors := f.directory + ",/"
	declared := "roles:\n  " + f.names.user + ":\n    login: true\n    password: \"Secret1!\"\n" +
		"resource_pools:\n  " + pool + ":\n    concurrent_query_limit: 2\n" +
		"grants:\n  pool:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_resource_pool: " + pool + "\n"
	poolGrants := func() []grantEntry {
		c.Helper()
		return grantsInSchemas(c, client, session, []string{f.directory, "/"}, []string{"resource pool"}, []string{f.names.user})
	}

	initial := hostPlanned(c, selectors, "yaml", declared)
	migrations, up, down := hostGenerate(c, selectors, "yaml", declared)
	applied, upErr := hostMigrate(c, f.directory, migrations, "migrations", "up")
	granted := poolGrants()
	settled := hostPlanned(c, selectors, "yaml", declared)
	rolledBack, downErr := hostMigrate(c, f.directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, fmt.Sprintf("GRANT 'ydb.granular.select_row' ON `.metadata/workload_manager/pools/%s` TO `%s`", pool, f.names.user))
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(granted, qt.DeepEquals, []grantEntry{{Role: f.names.user, Privilege: "ydb.granular.select_row", Kind: "resource pool", Name: pool}})
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(down, qt.Contains, "DROP RESOURCE POOL `"+pool+"`")
	c.Assert(down, qt.Not(qt.Contains), "REVOKE")
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(poolGrants(), qt.HasLen, 0)
	c.Assert(hostPlanned(c, selectors, "yaml", declared), qt.DeepEquals, initial)
}
