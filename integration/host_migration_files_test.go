//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"strings"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// The helpers in this file drive a migration through the host's files, as the
// host's rollback tests drive one through the generator's Go API:
// `migrations generate` writes the two files through this provider,
// `migrations up` and `migrations down` run them, and the revision table sits
// in the fixture directory, which the host leaves out of every comparison.

// hostGenerate runs the host's `migrations generate` over one source of
// format with this provider selected for ydb, restricted to the comma-separated
// selectors, such as the fixture directory, and with
// the further flags given, and returns the directory it wrote into with the
// up and the down text. The generation must succeed.
func hostGenerate(c *qt.C, selectors, format, text string, flags ...string) (migrations, up, down string) {
	c.Helper()
	root := c.TempDir()
	name := map[string]string{"go": "entities.go", "yaml": "schema.yaml", "sql": "schema.sql"}[format]
	c.Assert(os.WriteFile(filepath.Join(root, name), []byte(text), 0o600), qt.IsNil)
	source := []string{"--schema-file", filepath.Join(root, name)}
	if format == "go" {
		source = []string{"--root-dir", root}
	}
	migrations = c.TempDir()
	args := append([]string{"migrations", "generate", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-dir", migrations, "--schemas", selectors, "--name", "change"}, source...)
	output, err := hostCommand(c, append(args, flags...)...)
	c.Assert(err, qt.IsNil, qt.Commentf("migrations generate:\n%s", output))
	up, down = generatedPair(c, migrations)
	return migrations, up, down
}

// hostMigrate runs one versioned verb of the host over migrations, with the
// revision table in directory.
func hostMigrate(c *qt.C, directory, migrations string, verb ...string) (string, error) {
	c.Helper()
	return hostCommand(c, append(verb, "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-dir", migrations, "--migrations-schema", directory)...)
}

// generatedPair reads the up files and the down files a generation wrote
// into dir, each kind joined.
func generatedPair(c *qt.C, dir string) (string, string) {
	c.Helper()
	read := func(pattern string) string {
		names, err := filepath.Glob(filepath.Join(dir, pattern))
		c.Assert(err, qt.IsNil)
		var text strings.Builder
		for _, name := range names {
			body, err := os.ReadFile(name)
			c.Assert(err, qt.IsNil)
			text.Write(body)
		}
		return text.String()
	}
	return read("*.up.sql"), read("*.down.sql")
}

// objectsIn is the entries of the directory at absolute other than the
// revision tables a run writes there.
func objectsIn(held map[string]Ydb_Scheme.Entry_Type) map[string]Ydb_Scheme.Entry_Type {
	for name := range held {
		if strings.HasPrefix(name, "schema_migrations") {
			delete(held, name)
		}
	}
	return held
}
