//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBObjectAccess_AStreamingQuery: YQL declares
// topics, a user, a streaming query and a grant on it. The GRANT comes after
// the query. A changed query is altered in place and keeps its entries; a
// changed grant revokes one entry and grants another, the GRANT again last.
// Removing the query drops it without a REVOKE. The host's YQL prelude gives
// the user a password; a SQL schema reading has no home for one on the
// provider wire (see TestHostRefusesAPasswordInAYQLSchema_FailurePath), so
// the user is declared without it.
func TestHostGrantsOnAStreamingQuery_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	skipWithout(c, f.session, streaming.Capability)
	quoted := func(name string) string { return "`" + f.directory + "/" + name + "`" }
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP STREAMING QUERY `"+path.Join(f.database, f.directory, "copy")+"`")
		for _, name := range []string{"source", "sink"} {
			_ = execFixtureSchema(ctx, f.opened.DB, "DROP TOPIC `"+path.Join(f.database, f.directory, name)+"`")
		}
	})
	query := quoted("copy")
	prelude := "CREATE TOPIC " + quoted("source") + "; CREATE TOPIC " + quoted("sink") + "; CREATE USER " + f.names.user + ";"
	body := " AS DO BEGIN INSERT INTO " + quoted("sink") + " SELECT * FROM " + quoted("source")
	grant := func(permission string) string {
		return " GRANT '" + permission + "' ON " + query + " TO " + f.names.user + ";"
	}
	granted := func(permission string) string {
		return "GRANT '" + permission + "' ON " + query + " TO `" + f.names.user + "`"
	}
	want := func(permission string) []grantEntry {
		return []grantEntry{{Role: f.names.user, Privilege: permission, Kind: "streaming query", Name: f.directory + "/copy"}}
	}
	queryGrants := func() []grantEntry {
		c.Helper()
		return grantsIn(c, f.client, f.session, f.directory, []string{"streaming query"}, []string{f.names.user})
	}
	declared := prelude + "CREATE STREAMING QUERY " + query + " WITH (RUN=FALSE)" + body + "; END DO;" + grant("ydb.generic.manage")
	changed := prelude + "CREATE OR REPLACE STREAMING QUERY " + query + " WITH (RUN=FALSE)" + body + " WHERE TRUE; END DO;" + grant("ydb.generic.read")
	apply := func(name, text string) {
		c.Helper()
		output, err := hostApplySource(c, f.directory, "sql", text)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	}

	first := hostPlanned(c, f.directory, "sql", declared)
	apply("the query", declared)
	settled := hostPlanned(c, f.directory, "sql", declared)
	created := queryGrants()
	change := hostPlanned(c, f.directory, "sql", changed)
	apply("the change", changed)
	resettled := hostPlanned(c, f.directory, "sql", changed)
	altered := queryGrants()
	removal := hostPlanned(c, f.directory, "sql", prelude)
	apply("the removal", prelude)

	c.Assert(first[len(first)-1], qt.Equals, granted("ydb.generic.manage"))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(created, qt.DeepEquals, want("ydb.generic.manage"))
	c.Assert(change[len(change)-1], qt.Equals, granted("ydb.generic.read"))
	c.Assert(strings.Join(change, "\n"), qt.Contains, "REVOKE 'ydb.generic.manage' ON "+query+" FROM `"+f.names.user+"`")
	c.Assert(resettled, qt.HasLen, 0)
	c.Assert(altered, qt.DeepEquals, want("ydb.generic.read"))
	c.Assert(removal, qt.DeepEquals, []string{"streaming-query-operation " + f.directory + ".copy"})
	c.Assert(hostPlanned(c, f.directory, "sql", prelude), qt.HasLen, 0)
}

// Ported from the host's TestYDBObjectAccess_AResourcePool: the plan that
// creates a resource pool grants a user a permission on it, by the path YDB
// gives a pool; the read reports the entry and nothing is left to plan, and
// the entries YDB gave the pool default are reported as implicit. The host's
// test also drops the pool through the reverse plan of its generator; a
// source cannot drop a pool, which belongs to the whole database, so that
// part waits for a host migrations harness in this suite.
func TestHostGrantsOnAResourcePool_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := objectAccessFixture(c)
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
	principal := "roles:\n  " + f.names.user + ":\n    login: true\n    password: \"Secret1!\"\n"
	declared := principal + "resource_pools:\n  " + pool + ":\n    concurrent_query_limit: 2\n" +
		"grants:\n  pool:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_resource_pool: " + pool + "\n"
	poolGrants := func(roles ...string) []grantEntry {
		c.Helper()
		return grantsInSchemas(c, client, session, []string{f.directory, "/"}, []string{"resource pool"}, roles)
	}
	apply := func(name, text string) {
		c.Helper()
		output, err := hostApplySelected(c, selectors, "yaml", text)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	}

	created := hostPlanned(c, selectors, "yaml", declared)
	apply("the pool", declared)
	settled := hostPlanned(c, selectors, "yaml", declared)
	granted := poolGrants(f.names.user)
	defaults := poolGrants("all-users@well-known", "root@builtin")

	c.Assert(strings.Join(created, "\n"), qt.Contains, fmt.Sprintf("GRANT 'ydb.granular.select_row' ON `.metadata/workload_manager/pools/%s` TO `%s`", pool, f.names.user))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(granted, qt.DeepEquals, []grantEntry{{Role: f.names.user, Privilege: "ydb.granular.select_row", Kind: "resource pool", Name: pool}})
	c.Assert(defaults, qt.Not(qt.HasLen), 0)
	c.Assert(defaults, qt.All(qt.Satisfies), func(entry grantEntry) bool { return entry.Name != "default" || entry.Implicit })
}
