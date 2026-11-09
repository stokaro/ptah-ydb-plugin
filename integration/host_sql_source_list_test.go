//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

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
// TestLoadAll_AYQLFileCannotChangeWhatAnHCLSourceDeclares on every host;
// TestHostYQLSourceListExplainsTheHCLTable_FailurePath adds its explanation.
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

// hclUsers is the HCL source that declares the table users.
const hclUsers = "schema \"main\" {\n}\ntable \"users\" {\n  schema = schema.main\n  column \"id\" {\n    null = false\n    type = Int64\n  }\n" +
	"  primary_key {\n    columns = [column.id]\n  }\n}\n"

// Ported from the former host's
// TestLoadAll_AYQLFileCannotChangeWhatAnHCLSourceDeclares: a YQL file that
// adds a changefeed to a table only the HCL source of the same load declares
// is refused, and the refusal says the YQL files change only what the YQL
// files declare, naming the HCL file, as the built-in engine at 74de0b65b
// said through ptah-compat schema diff. The provider reports the statement as
// UNDECLARED under schema.read-sql-undeclared (contract 25.38), which the host
// reads as its own reader's refusal (sqlext.ErrUndeclared); before it the
// refusal was INVALID and the host could not explain it (G237). Native schema
// render explains it the same way.
func TestHostYQLSourceListExplainsTheHCLTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.SQLUndeclaredFeature)
	root := c.TempDir()
	users := compatFile(c, root, "users.hcl", hclUsers)
	feed := compatFile(c, root, "feed.sql", "ALTER TABLE users ADD CHANGEFEED updates WITH (mode = 'UPDATES', format = 'JSON');\n")
	explained := `(?s).*feed\.sql.*the schema model has no place for this statement: ALTER TABLE users ADD CHANGEFEED updates names a table this schema does not declare; ` +
		`a ydb SQL file changes only what the SQL files of the same load declare, not what users\.hcl declares.*`

	rendered, renderErr := hostCommand(c, "schema", "render", "--schema-file", filepath.FromSlash(users), "--schema-file", filepath.FromSlash(feed),
		"--dialect", "ydb", "--server-version", "26.2.1.14")
	_, compared, exit := runCompat(c, nil, "schema", "diff", "--from", "file://"+users, "--to", "file://"+users, "--to", "file://"+feed,
		"--dev-url", os.Getenv("PTAH_YDB_TEST_URL"))

	c.Assert(renderErr, qt.IsNotNil, qt.Commentf("%s", rendered))
	c.Assert(rendered, qt.Matches, explained)
	c.Assert(rendered, qt.Not(qt.Contains), "-- Statement ")
	c.Assert(exit, qt.Not(qt.Equals), 0, qt.Commentf("%s", compared))
	c.Assert(compared, qt.Matches, explained)
}
