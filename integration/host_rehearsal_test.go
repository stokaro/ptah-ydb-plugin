//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBBinary_DevDatabaseIsARealm rehearsal
// subtests: `schema apply --dev-url` names the target, so the rehearsal runs
// in a dev realm of the target database. The plan holds this provider's native
// operations, which are bound to the database they were planned for, so the
// host recreates the target's schema in the realm, plans the change again
// there and runs that plan through this provider before it applies the
// target's.

// rehearsalEntities is a Go source with the host test's table in directory,
// and fields the lines that follow its title.
func rehearsalEntities(directory, fields string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n" +
		"\t//ptah:schema:field name=\"title\" type=\"VARCHAR(80)\" not_null default=\"untitled\"\n\tTitle string\n" + fields + "}\n"
}

// rehearsalScore is the column the change below adds.
const rehearsalScore = "\t//ptah:schema:field name=\"score\" type=\"INT\"\n\tScore int32\n"

// rehearsalFixture is a locked directory the tests' table goes in, emptied
// when the test ends.
func rehearsalFixture(c *qt.C) (*connection.Connection, string, string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	return opened, directory, database
}

// rehearsedApply runs `schema apply` of entities to directory with the test
// database as both the target and the dev database.
func rehearsedApply(c *qt.C, entities, directory string) (string, error) {
	c.Helper()
	address := os.Getenv("PTAH_YDB_TEST_URL")
	return hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", entities, "--schemas", directory, "--dev-url", address, "--auto-approve")
}

// A table the target lacks is created in the realm first and then in the
// target, and the realm is gone when the host exits.
func TestHostRehearsalOnTheTargetsURLRunsInARealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalFixture(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	applied, err := rehearsedApply(c, entities, directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE})
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}

// A change to a table the target holds is rehearsed on a copy of that table:
// the host writes the table in the realm from the target's read, plans the
// added column there, and the target gets the column.
func TestHostRehearsalOfAChangeRecreatesTheTargetsTableInTheRealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalFixture(c)
	created, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--auto-approve",
		"--root-dir", writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")}))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	changed := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, rehearsalScore)})

	applied, err := rehearsedApply(c, changed, directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items")), qt.DeepEquals,
		map[string]string{"id": "Int64", "title": "Utf8", "score": "Optional<Int32>"})
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}

// The text of the two URLs is not compared on YDB, so the host compares the
// databases the two sessions select, and a dev database declared disposable
// gets no realm: it is the target, and the apply is refused before anything
// runs. The happy path above is the control, the same URLs without the
// declaration.
func TestHostRehearsalRefusesADisposableDevDatabaseThatIsTheTarget_FailurePath(t *testing.T) {
	t.Setenv("PTAH_DEV_SERVER_DISPOSABLE", "1")
	c := testcheck.New(t)
	opened, directory, database := rehearsalFixture(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	refused, err := rehearsedApply(c, entities, directory)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "--dev-url must not point at the target database")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}

// A dev realm on the target's own server sees the database's users, which
// belong to the whole database: the realm reads each one as existing out of
// its scope, and its attributes are not read. Recreating the target in the
// realm plans nothing for a user, the rehearsal passes, and the user is left
// as it was. The two-server test that refuses the user is
// TestHostRehearsalRefusesAUserItsRealmCannotHold_FailurePath.
func TestHostRehearsalInARealmLeavesTheDatabasesUsers_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalFixture(c)
	const user = "ptahrealmuser"
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "DROP USER IF EXISTS "+user), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE USER "+user), qt.IsNil)
	c.Cleanup(func() {
		c.Check(execFixtureSchema(context.Background(), opened.DB, "DROP USER IF EXISTS "+user), qt.IsNil)
	})
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	applied, err := rehearsedApply(c, entities, directory)
	var users uint64
	countErr := opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `.sys/auth_users` WHERE Sid = '"+user+"'").Scan(&users)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE})
	c.Assert(countErr, qt.IsNil)
	c.Assert(users, qt.Equals, uint64(1))
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}
