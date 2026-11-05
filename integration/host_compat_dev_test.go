//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
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

// devDockerDesired is a desired schema in HCL with one table more than the
// migration directory of TestHostCompatMigrateDiffOnADockerDevDatabase_HappyPath
// creates.
const devDockerDesired = `schema "app" {
}

table "users" {
  schema = schema.app
  column "id" {
    type = Int64
  }
  primary_key {
    columns = [column.id]
  }
}

table "orders" {
  schema = schema.app
  column "id" {
    type = Uint64
  }
  primary_key {
    columns = [column.id]
  }
}
`

// devContainers lists the containers a provider started for a dev database,
// by the label it gives each one. The daemon may be shared, so a test compares
// what a run leaves with what was there before it (see leftContainers) rather
// than the whole list: another run may remove its own container meanwhile.
func devContainers(c *qt.C) []string {
	c.Helper()
	listed, err := exec.CommandContext(c.Context(), "docker", "ps", "-aq", "--no-trunc", "--filter", "label="+devdatabase.ContainerLabel).Output()
	c.Assert(err, qt.IsNil)
	containers := strings.Fields(string(listed))
	slices.Sort(containers)
	return containers
}

// Ported from the host's TestDevDockerStartsYDBForTheCompatibilitySurfaceE2E:
// on the compatibility surface docker://ydb is a Ptah extension. The default
// profile hands it to this provider, which starts the dev database; migrate
// diff replays the directory there and writes a migration of only the table
// the directory lacks, and the run leaves no container. migrate diff plans in
// the exact statement style, which the provider honors
// (planning.statement-style).
func TestHostCompatMigrateDiffOnADockerDevDatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	binary, _ := hostCompat(c)
	manifest := providerManifestWith(c, dockerEnvironment())
	before := devContainers(c)
	dir := c.TempDir()
	compatFile(c, dir, "20260101000000_users.sql", "CREATE TABLE `app/users` (`id` Int64 NOT NULL, PRIMARY KEY (`id`));\n")
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", devDockerDesired)
	directory := "file://" + filepath.ToSlash(dir)

	hashed, hashNotes, hashExit := runCompatBinary(c, binary, manifest, nil, "migrate", "hash", "--dir", directory)
	diffed, diffNotes, diffExit := runCompatBinary(c, binary, manifest, nil, "migrate", "diff", "orders", "--dir", directory,
		"--to", desired, "--dev-url", "docker://ydb/"+devImageTag+"/local")
	written, globErr := filepath.Glob(filepath.Join(dir, "*_orders.sql"))

	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s\n%s", hashed, hashNotes))
	c.Assert(diffExit, qt.Equals, 0, qt.Commentf("migrate diff:\n%s\n%s", diffed, diffNotes))
	c.Assert(globErr, qt.IsNil)
	c.Assert(written, qt.HasLen, 1)
	body, err := os.ReadFile(written[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(body), qt.Contains, "CREATE TABLE `app/orders`")
	c.Assert(string(body), qt.Not(qt.Contains), "`app/users`")
	c.Assert(leftContainers(before, devContainers(c)), qt.HasLen, 0)
}

// leftContainers is the containers of after that before did not list.
func leftContainers(before, after []string) []string {
	return slices.DeleteFunc(slices.Clone(after), func(container string) bool { return slices.Contains(before, container) })
}
