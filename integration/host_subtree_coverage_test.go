//go:build integration

package integration_test

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// subtreeRealm is a dev realm of the test database on a line with secrets,
// whose objects go when the test ends, and the address of a session in it.
func subtreeRealm(c *qt.C) (address, root string, opened *connection.Connection) {
	c.Helper()
	opened, _, _ = secretsFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root = realmFixture(c, opened, base)
	c.Cleanup(func() { dropSecretsBelow(c, opened, root) })
	return address, root, opened
}

// declaredSecret writes a schema file that declares secret pw in directory,
// with its value read from variable, and returns its path.
func declaredSecret(c *qt.C, directory, variable string) string {
	c.Helper()
	source := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(source, []byte(fmt.Sprintf("secrets:\n  pw:\n    schema: %s\n    value_env: %s\n", directory, variable)), 0o600), qt.IsNil)
	return source
}

// Gap G41 of stokaro/ptah#4204: a read with no --schemas claims every
// directory below the root complete in one region, so `migrations generate`
// in a dev realm plans a secret declared in a directory that does not exist
// yet, as the host's embedded engine does: CREATE SECRET in the up file and
// DROP SECRET in the down file, and `migrations up` and `migrations down` run
// both there.
func TestHostPlansASecretInANewDirectoryOfAnUnfilteredRead_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	address, _, _ := subtreeRealm(c)
	migrations := c.TempDir()

	generated, generateErr := hostRunAt(c, address, "migrations", "generate", "--schema-file", declaredSecret(c, "fresh/nested", variable),
		"--migrations-dir", migrations, "--name", "secret")
	up, down := generatedPair(c, migrations)
	applied, upErr := hostRunAt(c, address, "migrations", "up", "--migrations-dir", migrations)
	rolledBack, downErr := hostRunAt(c, address, "migrations", "down", "--target", "0", "--confirm", "--migrations-dir", migrations)

	c.Assert(generateErr, qt.IsNil, qt.Commentf("migrations generate:\n%s", generated))
	c.Assert(generated, qt.Not(qt.Contains), "absence was not established")
	c.Assert(up, qt.Contains, "CREATE SECRET `fresh/nested/pw` WITH (value = $"+variable+");")
	c.Assert(down, qt.Contains, "DROP SECRET `fresh/nested/pw`;")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", rolledBack))
}

// A directory the walk leaves out, here a dot-prefixed one the realm holds,
// stays unknown below the root's complete claim: a secret declared in it is
// left unplanned with the warning, and no migration is written.
func TestHostLeavesASecretBelowASkippedDirectoryUnplanned_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	address, root, opened := subtreeRealm(c)
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(root, ".hidden")), qt.IsNil)
	migrations := c.TempDir()

	generated, err := hostRunAt(c, address, "migrations", "generate", "--schema-file", declaredSecret(c, ".hidden", variable),
		"--migrations-dir", migrations, "--name", "secret")
	written, globErr := filepath.Glob(filepath.Join(migrations, "*.sql"))

	c.Assert(err, qt.IsNil, qt.Commentf("migrations generate:\n%s", generated))
	c.Assert(generated, qt.Contains, "secret .hidden.pw could not be compared between the database and the desired schema: The current secret or its absence was not established")
	c.Assert(globErr, qt.IsNil)
	c.Assert(written, qt.HasLen, 0)
}
