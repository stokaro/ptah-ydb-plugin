//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the rotation half of the host's
// TestYDBBinary_KeepsASecretsValueOutOfEveryOutput: `migrations generate
// --rotate-secret` writes one ALTER SECRET into the up file, and a down file
// with nothing to run that says why: the value the rotation replaced was
// never read, so the rollback keeps the rotated one. `migrations up` and
// `migrations down` run both, and no output and no file holds the value.
func TestHostRotatesASecretThroughAMigration_HappyPath(t *testing.T) {
	first, second := secretValue(), secretValue()
	variable := secretVariable(t, first)
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	entities := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(entities, "schema.go"), []byte(secretsEntities(directory, variable, false)), 0o600), qt.IsNil)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	secret := directory + "/pg_password"
	applied, applyErr := hostRunAt(c, address, "schema", "apply", "--root-dir", entities, "--auto-approve", "--schemas", directory, "--tx-mode", "none")
	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	t.Setenv(variable, second)
	migrations := c.TempDir()

	generated, generateErr := hostRunAt(c, address, "migrations", "generate", "--root-dir", entities, "--schemas", directory, "--migrations-dir", migrations,
		"--name", "rotate", "--rotate-secret", secret)
	up, down := generatedPair(c, migrations)
	rotated, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(generateErr, qt.IsNil, qt.Commentf("migrations generate:\n%s", generated))
	c.Assert(strings.Count(up, "ALTER SECRET `"+secret+"` WITH (value = $"+variable+");"), qt.Equals, 1)
	c.Assert(down, qt.Contains, `"keep the secret and the value it holds"`)
	c.Assert(down, qt.Contains, "the value secret "+secret+" held before the rotation was never read; the rollback keeps the rotated value")
	c.Assert(down, qt.Not(qt.Contains), "ALTER SECRET")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", rotated))
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", rolledBack))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.Contains, "pg_password")
	for _, text := range []string{applied, generated, up, down, rotated, rolledBack} {
		c.Assert(text, qt.Not(qt.Contains), "SENTINEL")
	}
}
