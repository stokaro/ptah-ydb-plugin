//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// returningLines are what each certified line answers to UPDATE ...
// RETURNING on a table with a unique index, as the host's query builder
// renders it under returning_clause: 25.1 fails it, and 26.2 returns the row
// it wrote.
var returningLines = map[string]bool{"25.1": false, "26.2": true}

// serverLine is the release line of a server version, such as 26.2.
func serverLine(c *qt.C, version string) string {
	c.Helper()
	major, minor, ok := releaseline.Parse(version)
	c.Assert(ok, qt.IsTrue, qt.Commentf("server version %q", version))
	return fmt.Sprintf("%d.%d", major, minor)
}

// Ported from the host's TestYDBQueryBuilder_ReturningFollowsTheLine: a session
// reports returning_clause, and the key holds exactly where the server runs
// the clause. The host's query builder renders RETURNING only under the key,
// so a session that left it out lost the clause on every line, where it runs.
// INSERT ... RETURNING runs on both lines; UPDATE ... RETURNING on a table
// with a unique index is the statement that tells them apart.
func TestSessionReportsReturningWhereTheServerRunsIt(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	accounts := path.Join(database, directory, "accounts")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+accounts+"` (id Int64 NOT NULL, email Utf8, plan Utf8, PRIMARY KEY (id), "+
		"INDEX accounts_email GLOBAL UNIQUE SYNC ON (email))"), qt.IsNil)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	var inserted int64
	insertErr := opened.DB.QueryRowContext(c.Context(), "INSERT INTO `"+accounts+"` (`id`, `email`, `plan`) VALUES (1l, 'a@x'u, 'free'u) RETURNING `id`").Scan(&inserted)
	var plan string
	updateErr := opened.DB.QueryRowContext(c.Context(), "UPDATE `"+accounts+"` SET `plan` = 'paid'u WHERE `id` = 1l RETURNING `plan`").Scan(&plan)

	c.Assert(insertErr, qt.IsNil)
	c.Assert(inserted, qt.Equals, int64(1))
	held, reported := session.Capabilities["returning_clause"]
	c.Assert(reported, qt.IsTrue)
	c.Assert(held, qt.Equals, returningLines[serverLine(c, session.ServerVersion)])
	c.Assert(updateErr == nil, qt.Equals, held, qt.Commentf("server: %v", updateErr))
}
