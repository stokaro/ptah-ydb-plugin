//go:build integration

package integration_test

import (
	"context"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the view row of the host's
// TestGenerateMigrationAST_Replications_HappyPath
// (internal/planner/dialects/ydb): a source that declares an async replication
// and a view over the replica table the replication creates plans the
// replication first, since YDB refuses a view over a path nothing holds yet.
// The built-in engine at 74de0b65b planned the replication first on 25.1.4.7
// and 26.2.1.14; this provider placed the view first (gap G230).
//
// YDB creates the replica table some time after CREATE ASYNC REPLICATION
// returns, so the creation waits for it when the plan reads it next, and the
// one plan applies; without the wait the view failed (SCHEME_ERROR, issue
// 2003), with the built-in engine too (gap G231). Nothing is left to plan.
func TestHostCreatesAViewOverAReplicaAfterItsReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP VIEW `" + absolute("recent") + "`", "DROP ASYNC REPLICATION `" + absolute("mirror") + "` CASCADE",
			"DROP TABLE `" + absolute("replica") + "`", "DROP TABLE `" + absolute("src") + "`"} {
			_ = execFixtureSchema(cleanup, opened.DB, statement)
		}
	})
	tables := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"
	declared := tables + "async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + absolute("src") + "\n        target: " + path.Join(directory, "replica") + "\n" +
		"views:\n  recent:\n    name: " + directory + ".recent\n    body: \"SELECT id FROM `" + path.Join(directory, "replica") + "`\"\n"
	hostRun(c, directory, "the table", tables, `(?s).*Schema apply completed successfully.*`)

	planned := hostPlanned(c, directory, "yaml", declared)
	hostRun(c, directory, "the replication and the view", declared, `(?s).*Schema apply completed successfully.*`)

	c.Assert(planned, qt.DeepEquals, []string{"async-replication-operation " + directory + ".mirror", "CREATE VIEW `" + directory + "/recent` WITH (security_invoker = TRUE) AS"})
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"mirror", "recent", "replica", "src"})
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
}
