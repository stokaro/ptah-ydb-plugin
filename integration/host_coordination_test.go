//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
)

// hostApplySource runs the host's `schema apply` over one source with this
// provider selected for ydb, restricted to the fixture directory: a Go file
// under --root-dir for "go", and a schema file for "yaml" and "sql".
func hostApplySource(c *qt.C, directory, format, text string) (string, error) {
	c.Helper()
	return hostApplySelected(c, directory, format, text)
}

// hostApplySelected is hostApplySource over the comma-separated selectors
// given, such as a directory and the database root.
func hostApplySelected(c *qt.C, selectors, format, text string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	name := map[string]string{"go": "entities.go", "yaml": "schema.yaml", "sql": "schema.sql"}[format]
	c.Assert(os.WriteFile(filepath.Join(root, name), []byte(text), 0o600), qt.IsNil)
	source := []string{"--schema-file", filepath.Join(root, name)}
	if format == "go" {
		source = []string{"--root-dir", root}
	}
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	arguments := append([]string{"schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", selectors}, source...)
	cmd := exec.CommandContext(ctx, host, arguments...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// nodeSettings is what the server holds of a coordination node: the settings
// the fixtures declare.
type nodeSettings struct {
	SelfCheckMillis uint32
	ReadMode        coordination.ConsistencyMode
}

// describedNode reads a node back through the coordination service, or
// reports that the directory holds no entry of that name.
func describedNode(c *qt.C, opened *connection.Connection, database, directory, name string) (nodeSettings, bool) {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	found := false
	for _, entry := range listing.Children {
		found = found || entry.Name == name
	}
	if !found {
		return nodeSettings{}, false
	}
	_, config, err := opened.Driver.Coordination().DescribeNode(c.Context(), path.Join(database, directory, name))
	c.Assert(err, qt.IsNil)
	return nodeSettings{SelfCheckMillis: config.SelfCheckPeriodMillis, ReadMode: config.ReadConsistencyMode}, true
}

// goItems and sqlItems declare a table beside the node, in the fixture
// directory, so a source that declares no node still declares something.
const (
	goItems  = "//ptah:schema:table name=\"items\" schema=\"{dir}\"\ntype Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n}\n"
	sqlItems = "CREATE TABLE `{dir}/items` (id Uint64 NOT NULL, PRIMARY KEY (id));\n"
)

// The host plans a coordination node through this provider from each source
// that declares one: it creates the node, finds nothing to change on the next
// run, changes its settings in place, and removes it once the source no longer
// declares it. Every source describes the nodes of the directory completely.
// The host names a declared node without its database and the provider's read
// names the session's; the runs after the first compare the two.
func TestHostAppliesACoordinationNode_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format                   string
		strict, relaxed, removed string
	}{
		{format: "yaml",
			strict:  "coordination_nodes:\n  workers:\n    schema: {dir}\n    self_check_period: PT2S\n    read_consistency_mode: strict\n",
			relaxed: "coordination_nodes:\n  workers:\n    schema: {dir}\n    read_consistency_mode: relaxed\n",
			removed: "coordination_nodes: {}\n"},
		{format: "go",
			strict:  "package entities\n\n" + goItems + "\n//ptah:schema:coordinationnode name=\"workers\" schema=\"{dir}\" self_check_period=\"PT2S\" read_consistency_mode=\"strict\"\ntype Workers struct{}\n",
			relaxed: "package entities\n\n" + goItems + "\n//ptah:schema:coordinationnode name=\"workers\" schema=\"{dir}\" read_consistency_mode=\"relaxed\"\ntype Workers struct{}\n",
			removed: "package entities\n\n" + goItems},
		{format: "sql",
			strict:  sqlItems + "CREATE COORDINATION NODE `{dir}/workers` WITH (self_check_period = Interval('PT2S'), read_consistency_mode = 'strict');\n",
			relaxed: sqlItems + "CREATE COORDINATION NODE `{dir}/workers` WITH (read_consistency_mode = 'relaxed');\n",
			removed: sqlItems},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(database, directory, "workers"))
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
			})
			apply := func(name, text, want string) {
				c.Helper()
				output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(text, "{dir}", directory))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
			}
			apply("creation", test.strict, `(?s).*Schema apply completed successfully.*`)
			settings, found := describedNode(c, opened, database, directory, "workers")
			c.Assert(found, qt.IsTrue)
			c.Assert(settings, qt.Equals, nodeSettings{SelfCheckMillis: 2000, ReadMode: coordination.ConsistencyModeStrict})
			apply("a second run", test.strict, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a change", test.relaxed, `(?s).*Schema apply completed successfully.*`)
			settings, found = describedNode(c, opened, database, directory, "workers")
			c.Assert(found, qt.IsTrue)
			c.Assert(settings.ReadMode, qt.Equals, coordination.ConsistencyModeRelaxed)
			c.Assert(settings.SelfCheckMillis, qt.Not(qt.Equals), uint32(2000))
			apply("a run after the change", test.relaxed, `(?s).*Schema is synced, no changes to be made.*`)
			apply("the removal", test.removed, `(?s).*Schema apply completed successfully.*`)
			_, found = describedNode(c, opened, database, directory, "workers")
			c.Assert(found, qt.IsFalse)
		})
	}
}
