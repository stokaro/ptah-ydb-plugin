//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// hostApply runs the pinned host's `schema apply` over one Go source with this
// provider selected for ydb, against the test address, restricted to the
// fixture directory, and returns what the host printed and how it exited.
// YDB runs no DDL in a transaction, and the host requires the run to say so for
// a provider's steps.
func hostApply(c *qt.C, directory, source string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(root, "entities.go"), []byte(source), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", root, "--schemas", directory)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// hostItems declares items in the fixture directory, with the given further
// attributes of its table directive.
func hostItems(directory, attributes string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"" + attributes + "\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"ts\" type=\"Timestamp\"\n\tTS int64\n}\n"
}

// The host creates a row table through the provider, finds it as declared on
// the next run, and changes its TTL in place. The host names the table without
// its database while the provider's inspection names the session's, and every
// run after the first compares the two. Each run takes the provider's schema
// lock, whose node the lock fixture removes again.
func TestHostAppliesAndAltersARowTable_HappyPath(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
	})
	for _, run := range []struct{ name, attributes, want string }{
		{"creation", "", `(?s).*row-table-create-operation.*Schema apply completed successfully.*`},
		{"a second run", "", `(?s).*No changes planned.*`},
		{"a TTL", ` platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`, `(?s).*row-table-alter-operation.*Schema apply completed successfully.*`},
		{"a run after the TTL", ` platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`, `(?s).*No changes planned.*`},
	} {
		output, err := hostApply(c, directory, hostItems(directory, run.attributes))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
}
