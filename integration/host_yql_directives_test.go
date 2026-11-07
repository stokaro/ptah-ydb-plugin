//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// migrateOneFile applies a migration directory whose one up file is body
// through the host, in a fixture directory of the test's own, and returns
// what the host printed and its error.
func migrateOneFile(c *qt.C, body string) (string, error) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	migrations := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_probe.up.sql"), []byte(body), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_probe.down.sql"), []byte("SELECT 1;\n"), 0o600), qt.IsNil)
	return hostMigrate(c, directory, migrations, "migrations", "up")
}

// Ported from the host's TestBodies_YDBReadsYQLStrings and the YQL half of
// TestConservativeBodies_KeepsADirectiveBetweenMySQLSystemVariables: the host
// reads a YDB migration's directive lines by YQL's rules, so a directive
// line inside a multiline @@ string or a double-quoted string is string
// content. The last file holds the line between two @@ markers that a MySQL
// reading would take for system variables, and YQL reads as one string. Each
// line names a directive the host refuses, so a file applies only if the
// host left the line in the string.
func TestHostReadsDirectivesByYQLRules_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"inside a multiline string", "SELECT @@runbook\n-- +ptah nonsense\n@@ AS s;\n"},
		{"inside a double-quoted string", "SELECT \"a\\\"\n-- +ptah nonsense\n\" AS s;\n"},
		{"between two @@ a MySQL reading takes for variables", "SELECT @@a;\n-- +ptah nonsense\n@@ AS s;\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			out, err := migrateOneFile(c, test.body)

			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, "All migrations applied successfully")
		})
	}
}

// The same directive line outside any string is a directive, and the host
// refuses the file before it runs anything.
func TestHostReadsDirectivesByYQLRules_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	out, err := migrateOneFile(c, "-- +ptah nonsense\nSELECT 1;\n")

	c.Assert(err, qt.IsNotNil)
	c.Assert(out, qt.Contains, `invalid +ptah directive "nonsense"`)
}
