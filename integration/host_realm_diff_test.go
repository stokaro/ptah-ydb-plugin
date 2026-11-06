//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `schema diff` between the test database and a dev realm of it reads each
// side through this provider, which binds every value it reads to the
// database it read: the target's topic and node to the database, the realm's
// to the realm. The host moves the --to side's values to the --from side's
// database, which shares their root, and leaves the permissions on the
// database root out of both sides, as the rehearsal's baseline does. Without
// the move the provider refused the comparison (`YDB comparison has
// conflicting database contexts`, which the host printed as `provider compare
// models failed (InvalidArgument)`). The built-in YDB engine at host 74de0b65b
// planned each of these, on 25.1.4.7 and 26.2.1.14.

// realmDiffColumn is the column the realm's table holds beside the target's.
const realmDiffColumn = "      title: {type: Utf8}\n"

// realmDiffTarget applies a table, a topic and a coordination node in a
// directory of the test database, with the table's columns after its key in
// columns, and returns the database's address and the directory.
func realmDiffTarget(c *qt.C, columns string) (opened *connection.Connection, target, directory, database string) {
	c.Helper()
	opened, directory, database = rehearsalFixture(c)
	target = os.Getenv("PTAH_YDB_TEST_URL")
	applyRealmDiffObjects(c, target, directory, columns)
	return opened, target, directory, database
}

// applyRealmDiffObjects applies the table, the topic and the node of
// [realmDiffTarget] to the directory of the database address names.
func applyRealmDiffObjects(c *qt.C, address, directory, columns string) {
	c.Helper()
	source := writeFiles(c, c.TempDir(), map[string]string{"s.yaml": rehearsalObjects(directory, columns)}) + "/s.yaml"
	applied, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory, "--schema-file", source, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
}

// realmDiffFixture is [realmDiffTarget] with targetColumns and a dev realm of
// the test database whose directory of the same name holds the same objects
// with realmColumns. It returns the target's address, the realm's and the
// directory, and removes the realm when the test ends.
func realmDiffFixture(c *qt.C, targetColumns, realmColumns string) (target, realm, directory string) {
	c.Helper()
	opened, target, directory, database := realmDiffTarget(c, targetColumns)
	name := "r" + strings.ToLower(rand.Text())[:12]
	parsed, err := url.Parse(target)
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	query.Set("dev_realm", name)
	parsed.RawQuery = query.Encode()
	realm = parsed.String()
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(database, connection.RealmDirectory, name))
		// Another test may hold a realm of its own, which keeps the directory.
		_ = opened.Driver.Scheme().RemoveDirectory(ctx, path.Join(database, connection.RealmDirectory))
	})
	applyRealmDiffObjects(c, realm, directory, realmColumns)
	return target, realm, directory
}

// The diff from the target to its realm adds the realm's column, and the diff
// back drops it. Neither revokes the database root's permissions or grants
// them on the realm's root, which the built-in engine did in the second
// direction: a realm's root does not stand in for its database's.
func TestHostDiffsATargetAndItsDevRealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	target, realm, directory := realmDiffFixture(c, "", realmDiffColumn)

	added, addErr := hostCommand(c, "schema", "diff", "--from", target, "--to", realm, "--schemas", directory)
	dropped, dropErr := hostCommand(c, "schema", "diff", "--from", realm, "--to", target, "--schemas", directory)

	c.Assert(addErr, qt.IsNil, qt.Commentf("%s", added))
	c.Assert(previewStatements(added), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` ADD COLUMN `title` Utf8;"}, qt.Commentf("%s", added))
	c.Assert(dropErr, qt.IsNil, qt.Commentf("%s", dropped))
	c.Assert(previewStatements(dropped), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` DROP COLUMN `title`;"}, qt.Commentf("%s", dropped))
}

// ptah-compat's `schema diff` between the target and its realm plans the same.
func TestHostCompatDiffsATargetAndItsDevRealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	target, realm, directory := realmDiffFixture(c, "", realmDiffColumn)

	added, addNotes, addExit := runCompat(c, nil, "schema", "diff", "--from", target, "--to", realm, "--schema", directory)
	dropped, dropNotes, dropExit := runCompat(c, nil, "schema", "diff", "--from", realm, "--to", target, "--schema", directory)

	c.Assert(addExit, qt.Equals, 0, qt.Commentf("%s\n%s", added, addNotes))
	c.Assert(previewStatements(added), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` ADD COLUMN `title` Utf8;"}, qt.Commentf("%s", added))
	c.Assert(dropExit, qt.Equals, 0, qt.Commentf("%s\n%s", dropped, dropNotes))
	c.Assert(previewStatements(dropped), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` DROP COLUMN `title`;"}, qt.Commentf("%s", dropped))
}

// A --from file beside a --to database is created in a dev realm of the
// --dev-url database and read back there, so the comparison holds the realm's
// read and the target's: the diff from the file to the target adds the
// column the target holds and the file leaves out.
func TestHostDiffsADeclarationCreatedInADevRealmWithTheTarget_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, target, directory, _ := realmDiffTarget(c, realmDiffColumn)
	declared := writeFiles(c, c.TempDir(), map[string]string{"s.yaml": rehearsalObjects(directory, "")}) + "/s.yaml"

	diffed, err := hostCommand(c, "schema", "diff", "--from", declared, "--to", target, "--schemas", directory, "--dev-url", target)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", diffed))
	c.Assert(previewStatements(diffed), qt.DeepEquals, []string{"ALTER TABLE `" + directory + "/items` ADD COLUMN `title` Utf8;"}, qt.Commentf("%s", diffed))
}
