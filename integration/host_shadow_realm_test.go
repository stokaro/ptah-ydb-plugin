//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// generatedFile reads the one up file a generation named name wrote into
// migrations.
func generatedFile(c *qt.C, migrations, name string) string {
	c.Helper()
	names, err := filepath.Glob(filepath.Join(migrations, "*_"+name+".up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(names, qt.HasLen, 1)
	body, err := os.ReadFile(names[0])
	c.Assert(err, qt.IsNil)
	return string(body)
}

// Ported from the host's TestYDBSecrets_ShadowVerifiesARotation: `migrations
// generate --rotate-secret --shadow-db`, with the target's own address as the
// shadow, verifies the rotation in a dev realm of the target. The replay
// creates the secret from the history and rotates it, and the convergence
// check compares the realm with the sources bound for the realm, so it reads
// the replayed secret as its declaration rather than planning the rotation
// again. The up file holds one ALTER SECRET, and the realm is gone after the
// run.
func TestHostShadowVerifiesARotation_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	entities := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(entities, "schema.go"), []byte(secretsEntities(directory, variable)), 0o600), qt.IsNil)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	secret := directory + "/pg_password"
	migrations := c.TempDir()
	created, err := hostRunAt(c, address, "migrations", "generate", "--root-dir", entities, "--schemas", directory, "--migrations-dir", migrations, "--name", "secret")
	c.Assert(err, qt.IsNil, qt.Commentf("migrations generate:\n%s", created))
	applied, err := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(err, qt.IsNil, qt.Commentf("up:\n%s", applied))
	t.Setenv(variable, secretValue())

	rotated, rotateErr := hostRunAt(c, address, "migrations", "generate", "--root-dir", entities, "--schemas", directory, "--migrations-dir", migrations,
		"--name", "rotate", "--rotate-secret", secret, "--shadow-db", address)

	c.Assert(rotateErr, qt.IsNil, qt.Commentf("migrations generate:\n%s", rotated))
	c.Assert(strings.Count(generatedFile(c, migrations, "rotate"), "ALTER SECRET `"+secret+"` WITH (value = $"+variable+");"), qt.Equals, 1)
	c.Assert(listedNames(c, opened, database), qt.Not(qt.Contains), connection.RealmDirectory)
	c.Assert(rotated, qt.Not(qt.Contains), "SENTINEL")
}

// Ported from the host's "a shadow database that is the target is a realm in
// it": `migrations generate --shadow-db`, with the target's own address as the
// shadow, writes the migration of a table in the directory it selects after
// verifying it in a dev realm of the target, where the replay creates the
// directory, and leaves the target without the table and without the realm.
func TestHostShadowDatabaseThatIsTheTargetIsARealmInIt_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	declared := "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      label: {type: Utf8}\n"

	_, up, _ := hostGenerate(c, directory, "yaml", declared, "--shadow-db", os.Getenv("PTAH_YDB_TEST_URL"))

	c.Assert(up, qt.Contains, "CREATE TABLE `"+directory+"/items`")
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.Not(qt.Contains), "items")
	c.Assert(listedNames(c, opened, database), qt.Not(qt.Contains), connection.RealmDirectory)
}
