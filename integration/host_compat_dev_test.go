//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBCompatBinary_DevDatabaseOnAnotherServer, the
// compatibility surface's form of TestHostDevDatabaseOnAnotherServer_HappyPath:
// the dev URL names a second server, whose database is /local too. schema
// apply rehearses its plan in a dev realm there, and migrate down verifies the
// rollback in one, where a comparison of the two URLs as written could not
// tell the servers apart. The target is a dev realm of the test database, so
// the schema apply of a whole document leaves the rest of the server alone,
// and the second server keeps nothing.
func TestHostCompatDevDatabaseOnAnotherServer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	devURL, other := otherServer(c)
	realm := newCLIRealm(c)
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", compatDesired)
	dir := compatMigrationsDir(c)

	applied, applyNotes, applyExit := runCompat(c, nil, "schema", "apply", "--url", realm.address, "--to", desired, "--dev-url", devURL, "--auto-approve")
	hashed, hashNotes, hashExit := runCompat(c, nil, "migrate", "hash", "--dir", dir)
	migrated, migrateNotes, migrateExit := runCompat(c, nil, "migrate", "apply", "--url", realm.address, "--dir", dir, "--revisions-schema", compatDirectory)
	rolledBack, downNotes, downExit := runCompat(c, nil, "migrate", "down", "--url", realm.address, "--dir", dir, "--revisions-schema", compatDirectory,
		"--to-version", "20260101000000", "--dev-url", devURL)

	c.Assert(applyExit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", applied, applyNotes))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s\n%s", hashed, hashNotes))
	c.Assert(migrateExit, qt.Equals, 0, qt.Commentf("migrate apply:\n%s\n%s", migrated, migrateNotes))
	c.Assert(downExit, qt.Equals, 0, qt.Commentf("migrate down:\n%s\n%s", rolledBack, downNotes))
	c.Assert(rolledBack, qt.Contains, "Rollback plan verified on shadow database")
	c.Assert(heldColumnTypes(c, realm.opened, path.Join(realm.root, compatDirectory, "orders")), qt.DeepEquals,
		map[string]string{"id": "Uint64", "total": "Decimal(22,9)"})
	c.Assert(heldColumnTypes(c, realm.opened, path.Join(realm.root, compatDirectory, "items")), qt.DeepEquals, map[string]string{"id": "Int64"})
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), connection.RealmDirectory)
	c.Assert(entryNames(heldEntries(c, other, other.Driver.Name())), qt.Not(qt.Contains), compatDirectory)
}
