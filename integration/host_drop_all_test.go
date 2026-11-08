//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestWriter_DropAllTables (internal/dbschema/ydb): db
// drop-all of a dev realm drops the row tables, column tables, views, topics,
// external tables, external data sources and coordination nodes of every
// directory in it, a table whose name holds a backtick and a node named as
// the lock node in a directory below the root included, and removes the
// directories. It keeps a node whose name starts with a dot and the realm's
// own lock node. The dry run lists those objects, the directories included,
// and changes nothing.
//
// The built-in engine kept a directory that was empty before the run; this
// provider removes it too. A dev cleanup lists an empty directory as an object
// (devclean.Walk), so a dev database that keeps one is not clean, and the
// provider's database cleanup is the dev cleanup.
func TestHostDropAllEmptiesARealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	emptyAtCleanup(c, opened, root)
	at := func(name string) string { return "`" + path.Join(root, name) + "`" }
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(root, "keep")), qt.IsNil)
	for _, statement := range []string{
		"CREATE TABLE " + at("t1") + " (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE EXTERNAL DATA SOURCE " + at("app/s3") + " WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.test/drop/', AUTH_METHOD = 'NONE')",
		"CREATE TABLE `" + path.Join(root, "app/sub") + "/t\\`3` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE " + at("app/t2") + " (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE VIEW " + at("app/v2") + " WITH (security_invoker = TRUE) AS SELECT 1 AS one",
		"CREATE TABLE " + at("mixed/t4") + " (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TOPIC " + at("mixed/events"),
		"CREATE TABLE " + at("olap") + " (id Int64 NOT NULL, PRIMARY KEY (id)) PARTITION BY HASH(id) WITH (STORE = COLUMN)",
		"CREATE TOPIC " + at("queues/events"),
		"CREATE TABLE " + at("queues/olap") + " (id Int64 NOT NULL, PRIMARY KEY (id)) PARTITION BY HASH(id) WITH (STORE = COLUMN)",
		"CREATE VIEW " + at("v") + " WITH (security_invoker = TRUE) AS SELECT 1 AS one",
		"CREATE EXTERNAL TABLE " + at("z/files") + " (id Int64 NOT NULL) WITH (DATA_SOURCE = '" + path.Join(root, "app/s3") + "', LOCATION = '/', FORMAT = 'json_each_row')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	for _, node := range []string{".hidden", "locks", "nodes/ptah_locks", "ptah_locks"} {
		c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), path.Join(root, node), coordination.NodeConfig{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 10000}), qt.IsNil)
	}
	before := listedNames(c, opened, root)

	planned, planErr := hostRunAt(c, address, "db", "drop-all", "--dry-run")
	afterPlan := listedNames(c, opened, root)
	dropped, dropErr := hostRunAt(c, address, "db", "drop-all", "--auto-approve")

	c.Assert(before, qt.DeepEquals, []string{".hidden", "app", "keep", "locks", "mixed", "nodes", "olap", "ptah_locks", "queues", "t1", "v", "z"})
	c.Assert(planErr, qt.IsNil, qt.Commentf("%s", planned))
	c.Assert(slices.DeleteFunc(strings.Split(planned, "\n"), func(line string) bool { return !strings.HasPrefix(line, "- ") }), qt.DeepEquals, []string{
		`- column table "olap"`, `- column table "olap" in schema "queues"`,
		`- coordination node "locks"`, `- coordination node "ptah_locks" in schema "nodes"`,
		`- directory "app"`, `- directory "keep"`, `- directory "mixed"`, `- directory "nodes"`, `- directory "queues"`, `- directory "z"`,
		`- directory "sub" in schema "app"`,
		`- external data source "s3" in schema "app"`, `- external table "files" in schema "z"`,
		`- table "t1"`, `- table "t2" in schema "app"`, "- table \"t`3\" in schema \"app/sub\"", `- table "t4" in schema "mixed"`,
		`- topic "events" in schema "mixed"`, `- topic "events" in schema "queues"`,
		`- view "v"`, `- view "v2" in schema "app"`,
	})
	c.Assert(afterPlan, qt.DeepEquals, before)
	c.Assert(dropErr, qt.IsNil, qt.Commentf("%s", dropped))
	c.Assert(listedNames(c, opened, root), qt.DeepEquals, []string{".hidden", "ptah_locks"})
}
