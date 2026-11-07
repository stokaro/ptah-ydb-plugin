//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// yqlIndentSchema is a desired YQL schema whose view query has a multiline
// string and, after it, a continuation line outside any string.
const yqlIndentSchema = "CREATE TABLE `app/t` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
	"CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS\nSELECT @@first\n  second@@ AS s,\n  id FROM `app/t`;\n"

// Ported from the YQL cases of the host's
// TestMigrateDiffIndentPreservesQuotedSQL and
// TestMigrateDiffIndentReadsTheStatementsDialect: `migrate diff --format '{{
// sql . "\t" }}'` indents a statement's continuation lines and leaves the
// lines inside a quoted value as they are, reading the statement as YQL.
// Read as T-SQL, @@ starts a global variable, and the line inside the string
// would be indented as code. The line inside the multiline string keeps its
// two spaces, and the line after the string gets the tab.
func TestHostCompatMigrateDiffIndentsYQLAsYQL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	dir := c.TempDir()
	desired := "file://" + compatFile(c, c.TempDir(), "schema.sql", yqlIndentSchema)

	out, notes, exit := runCompat(c, nil, "migrate", "diff", "views", "--dir", "file://"+filepath.ToSlash(dir), "--to", desired,
		"--dev-url", realm.address, "--format", `{{ sql . "\t" }}`)
	written, err := filepath.Glob(filepath.Join(dir, "*_views.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(written, qt.HasLen, 1)
	content, err := os.ReadFile(written[0])

	c.Assert(exit, qt.Equals, 0, qt.Commentf("migrate diff:\n%s\n%s", out, notes))
	c.Assert(err, qt.IsNil)
	c.Assert(string(content), qt.Contains, "CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS\nSELECT @@first\n  second@@ AS s,\n\tid FROM `app/t`\n;\n")
}
