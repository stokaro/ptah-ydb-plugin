//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// otherServerVariable names a second YDB server, for the tests whose dev
// database must be on a server other than the target's. Both servers name
// their database /local, so the database path cannot tell the two addresses
// apart, and the host cannot either: one YDB database answers on every node of
// its cluster.
const otherServerVariable = "PTAH_YDB_OTHER_TEST_URL"

// otherServer opens the second server and returns its address and connection,
// or skips the test where none is configured.
func otherServer(c *qt.C) (string, *connection.Connection) {
	c.Helper()
	raw := os.Getenv(otherServerVariable)
	if raw == "" {
		c.Skipf("%s names no second YDB server", otherServerVariable)
	}
	address, err := connection.Parse(raw)
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	return raw, opened
}

// Ported from the host's TestYDBBinary_ARehearsalRefusesAUserItsRealmCannotHold:
// the target holds a user the other server does not, so recreating the target
// in a realm there would create a user of the other server's whole database,
// which removing the realm leaves behind. The rehearsal refuses before any
// statement runs and names the statement, and the target is not changed. A dev
// realm on the target's own server sees the user and needs to create nothing,
// which is why the test needs the second server.
func TestHostRehearsalRefusesAUserItsRealmCannotHold_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherServer(c)
	opened, directory, database := rehearsalFixture(c)
	const user = "ptahbaselineuser"
	// An interrupted run may have left the user, and the cleanup drops it
	// before anything it asserts can stop it.
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "DROP USER IF EXISTS "+user), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE USER "+user), qt.IsNil)
	c.Cleanup(func() {
		c.Check(execFixtureSchema(context.Background(), opened.DB, "DROP USER IF EXISTS "+user), qt.IsNil)
	})
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	refused, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities, "--schemas", directory,
		"--dev-url", devURL, "--auto-approve")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Matches, `(?s).*baseline statement \d+ \((CREATE|ALTER) (USER|GROUP) [^\n]*\) cannot be rehearsed: `+
		`ydb rehearsal baseline refuses a (user|group) of the whole database because its effects cannot be confined to the dev database realm; `+
		`if nothing else uses this server, declare it disposable with PTAH_DEV_SERVER_DISPOSABLE=1.*`)
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.HasLen, 0)
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
}

// Ported from the host's TestYDBBinary_DevDatabaseOnAnotherServer: the dev
// and shadow URLs name the other server, so each one a run compares with the
// target names a dev realm there, which no comparison confuses with the
// target's database. The rollback verification, the derived rollback and the
// rehearsal all run there, and the other server keeps nothing.
func TestHostDevDatabaseOnAnotherServer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherServer(c)
	fixture := newMigrationsFixture(c)
	opened, directory, database := fixture.opened, path.Dir(fixture.dir), fixture.database
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_notes.up.sql":   "CREATE TABLE `" + fixture.table("notes") + "` (`id` Int64 NOT NULL, PRIMARY KEY (`id`));\n",
		"0000000001_notes.down.sql": "DROP TABLE `" + fixture.table("notes") + "`;\n",
	})
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})
	run := func(args ...string) (string, error) {
		return hostCommand(c, append(args, fixture.target(migrations)...)...)
	}

	up, upErr := run("migrations", "up")
	verified, verifyErr := run("migrations", "down", "--target", "0", "--shadow-db", devURL, "--confirm")
	upAgain, upAgainErr := run("migrations", "up")
	derived, deriveErr := run("migrations", "down", "--target", "0", "--plan", "--shadow-db", devURL, "--confirm")
	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities,
		"--schemas", directory, "--dev-url", devURL, "--auto-approve")

	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", up))
	c.Assert(verifyErr, qt.IsNil, qt.Commentf("%s", verified))
	c.Assert(verified, qt.Contains, "Rollback plan verified on shadow database")
	c.Assert(verified, qt.Contains, "Database is now at version: 0")
	c.Assert(upAgainErr, qt.IsNil, qt.Commentf("%s", upAgain))
	c.Assert(deriveErr, qt.IsNil, qt.Commentf("%s", derived))
	c.Assert(derived, qt.Contains, "DROP TABLE `"+fixture.table("notes")+"`")
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals,
		map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE, path.Base(fixture.dir): Ydb_Scheme.Entry_DIRECTORY})
	c.Assert(fixture.names(c), qt.Not(qt.Contains), "notes")
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), directory)
}
