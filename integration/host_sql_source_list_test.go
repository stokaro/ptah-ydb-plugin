//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// YQL files in one source list can change a table another YQL file in the
// list declares. This ports the former host's
// TestLoadAll_AYQLFileChangesWhatTheLoadDeclares through schema render.
func TestHostYQLSourceListCarriesAChangefeed_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, "stokaro/ptah#5743, which reads a provider's SQL source list as one load")
	root := c.TempDir()
	dir := writeFiles(c, filepath.Join(root, "tables"), map[string]string{
		"001.sql": "CREATE TABLE users (id Int64 NOT NULL, PRIMARY KEY (id));\n",
	})
	file := filepath.Join(writeFiles(c, root, map[string]string{
		"feed.sql": "ALTER TABLE users ADD CHANGEFEED updates WITH (mode = 'UPDATES', format = 'JSON');\n",
	}), "feed.sql")

	out, err := hostCommand(c, "schema", "render", "--schema-file", dir, "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2.1.14")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "CREATE TABLE `users`")
	c.Assert(out, qt.Contains, "ADD CHANGEFEED `updates`")
}

// A YQL file cannot change a table named only by the HCL source of the same
// load. This covers the refusal in the former host's
// TestLoadAll_AYQLFileCannotChangeWhatAnHCLSourceDeclares. Its extra
// source-format explanation still needs a distinct provider diagnostic code.
func TestHostYQLSourceListRefusesChangingAnHCLTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	root := c.TempDir()
	writeFiles(c, root, map[string]string{
		"users.hcl": "schema \"main\" {\n}\ntable \"users\" {\n  schema = schema.main\n  column \"id\" {\n    null = false\n    type = Int64\n  }\n  primary_key {\n    columns = [column.id]\n  }\n}\n",
		"feed.sql":  "ALTER TABLE users ADD CHANGEFEED updates WITH (mode = 'UPDATES', format = 'JSON');\n",
	})

	out, err := hostCommand(c, "schema", "render", "--schema-file", filepath.Join(root, "users.hcl"),
		"--schema-file", filepath.Join(root, "feed.sql"), "--dialect", "ydb", "--server-version", "26.2.1.14")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "names a table this schema does not declare")
	c.Assert(out, qt.Contains, "users.hcl")
	c.Assert(out, qt.Not(qt.Contains), "-- Statement ")
}
