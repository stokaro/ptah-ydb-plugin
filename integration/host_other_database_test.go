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

// otherDatabaseVariable names a second database on the test server, one of
// another name than the test database's. Its authored references are read
// against another root, so a dev realm there has a context that shares
// nothing with the target's: this provider binds the target's values to the
// target, and the host has to move them to the realm across roots before the
// realm compares them. Without that move the provider refused the comparison
// (`YDB comparison has conflicting database contexts`, which the host printed
// as `provider compare models failed (InvalidArgument)`). The built-in YDB
// engine at host 74de0b65b rehearsed and diffed each of these on 25.1.4.7 and
// 26.2.1.14. local-ydb serves one database; CI serves this one from a dynamic
// node of the test server (scripts/second-database.sh).
const otherDatabaseVariable = "PTAH_YDB_OTHER_DATABASE_TEST_URL"

// otherDatabase opens the second database and returns its address and
// connection, or skips the test where none is configured.
func otherDatabase(c *qt.C) (string, *connection.Connection) {
	c.Helper()
	raw := os.Getenv(otherDatabaseVariable)
	if raw == "" {
		c.Skipf("%s names no second YDB database", otherDatabaseVariable)
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

// otherDatabaseSource writes rehearsalObjects(directory, columns), a table, a
// topic and a coordination node, and returns the file's path.
func otherDatabaseSource(c *qt.C, directory, columns string) string {
	c.Helper()
	return writeFiles(c, c.TempDir(), map[string]string{"s.yaml": rehearsalObjects(directory, columns)}) + "/s.yaml"
}

// A change to a directory that holds a topic and a coordination node is
// rehearsed in a realm of the other database: the host moves the target's
// values there, the realm recreates the table, the topic and the node, and
// the target gets the column. The other database keeps nothing.
func TestHostRehearsesInADevDatabaseOfAnotherName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherDatabase(c)
	opened, directory, database := rehearsalFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	created, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory, "--schema-file", otherDatabaseSource(c, directory, ""), "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))

	applied, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory,
		"--schema-file", otherDatabaseSource(c, directory, "      title: {type: Utf8}\n"), "--dev-url", devURL, "--auto-approve")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{
		"items": Ydb_Scheme.Entry_TABLE, "events": Ydb_Scheme.Entry_TOPIC, "workers": Ydb_Scheme.Entry_COORDINATION_NODE,
	})
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items")), qt.DeepEquals, map[string]string{"id": "Uint64", "title": "Optional<Utf8>"})
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
}

// With resource pools on, the target holds the pool default, which a realm of
// the other database does not read: it is left out of what the realm
// recreates, and a new table is rehearsed and created.
func TestHostRehearsesInADevDatabaseOfAnotherNameWithResourcePoolsOn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherDatabase(c)
	opened, directory, database := rehearsalWithPools(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	applied, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities, "--schemas", directory,
		"--dev-url", devURL, "--auto-approve")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE})
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
}

// `ptah-compat schema apply --dev-url` names the other database and rehearses
// the same way, from an SQL source, with resource pools on.
func TestHostCompatRehearsesInADevDatabaseOfAnotherName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherDatabase(c)
	opened, directory, database := rehearsalWithPools(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	desired := "file://" + compatFile(c, c.TempDir(), "items.sql", "CREATE TABLE `"+directory+"/items` (id Int64 NOT NULL, title Utf8, PRIMARY KEY (id));\n")

	applied, notes, exit := runCompat(c, nil, "schema", "apply", "--url", address, "--to", desired, "--schema", directory, "--dev-url", devURL, "--auto-approve")

	c.Assert(exit, qt.Equals, 0, qt.Commentf("%s\n%s", applied, notes))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items")), qt.DeepEquals, map[string]string{"id": "Int64", "title": "Optional<Utf8>"})
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
}

// `schema diff --from <file> --to <database> --dev-url <other database>`
// creates the file in a realm of the other database and reads it back there;
// the host moves the target's values to that realm, and the diff adds the
// column the target holds and the file leaves out.
func TestHostDiffsADeclarationCreatedInADevDatabaseOfAnotherName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	devURL, other := otherDatabase(c)
	_, directory, _ := rehearsalFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	created, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory,
		"--schema-file", otherDatabaseSource(c, directory, "      title: {type: Utf8}\n"), "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))

	diffed, err := hostCommand(c, "schema", "diff", "--from", otherDatabaseSource(c, directory, ""), "--to", address, "--schemas", directory, "--dev-url", devURL)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", diffed))
	c.Assert(previewStatements(diffed), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` ADD COLUMN `title` Utf8;"}, qt.Commentf("%s", diffed))
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
}
