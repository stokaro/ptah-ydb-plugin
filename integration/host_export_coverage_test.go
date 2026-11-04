//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
)

// The tests here export a read of one directory through the host's own
// writers, `ptah introspect` and `ptah schema inspect --format hcl`, and apply
// the export back over that directory and a second one the read did not
// list. A read knows a native kind only in the directories it listed, so the
// export says the kind is not described, and applying it removes nothing of
// the second directory. Neither directory holds a table the other lacks: the
// host plans the tables of a directory the export does not describe on its
// own, and this provider has no say in it.

// outsideDirectory makes a second directory beside a fixture's and removes it
// when the test ends.
func outsideDirectory(c *qt.C, opened *connection.Connection, database, directory string) string {
	c.Helper()
	outside := directory + "_outside_" + strings.ToLower(rand.Text()[:6])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Assert(opened.Driver.Scheme().MakeDirectory(ctx, path.Join(database, outside)), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(database, outside)), qt.IsNil)
	})
	return outside
}

// createSchemaObject runs create, and drop when the test ends.
func createSchemaObject(c *qt.C, opened *connection.Connection, create, drop string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, opened.DB, create), qt.IsNil, qt.Commentf("%s", create))
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, drop), qt.IsNil, qt.Commentf("%s", drop))
	})
}

// createNode creates the coordination node at absolute, and drops it when the
// test ends.
func createNode(c *qt.C, opened *connection.Connection, absolute string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Assert(opened.Driver.Coordination().CreateNode(ctx, absolute, coordination.NodeConfig{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 10000}), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Coordination().DropNode(cleanup, absolute), qt.IsNil)
	})
}

// entries are the names a directory holds.
func entries(c *qt.C, opened *connection.Connection, database, directory string) []string {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	var names []string
	for _, entry := range listing.Children {
		names = append(names, entry.Name)
	}
	return names
}

// hostExport runs one of the host's commands over the directory with this
// provider selected for ydb, and returns its standard output and what it
// reported.
func hostExport(c *qt.C, arguments ...string) (string, string) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, append(arguments, "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"))...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s\n%s", stdout.String(), stderr.String()))
	return stdout.String(), stderr.String()
}

// A Go export of a read of one directory leaves the native kinds the read
// knew only there not described, and applied back over a second directory it
// removes none of that directory's topics or coordination nodes.
func TestHostReappliesAGoExportOfANarrowedReadSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	events, stream, feed := path.Join(database, directory, "events"), path.Join(database, directory, "stream"), path.Join(database, outside, "feed")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))", "DROP TABLE `"+events+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+stream+"` (CONSUMER reader)", "DROP TOPIC `"+stream+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+feed+"` (CONSUMER reader)", "DROP TOPIC `"+feed+"`")
	createNode(c, opened, path.Join(database, outside, "workers"))
	out := c.TempDir()

	hostExport(c, "introspect", "--schemas", directory, "--out", out, "--single-file")

	written, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	source := string(written)
	for _, kind := range []string{"topic", "coordination_node", "secret", "streaming_query", "external_data_source", "external_table"} {
		c.Assert(source, qt.Contains, `//ptah:schema:notdescribed kind="`+kind+`" reason="not-inspected" provenance="observed"`, qt.Commentf("%s", source))
	}
	c.Assert(source, qt.Contains, `//ptah:schema:topic name="stream" schema="`+directory+`"`, qt.Commentf("%s", source))
	output, err := hostApplySelected(c, directory+","+outside, "go", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", source, output))
	c.Assert(entries(c, opened, database, outside), qt.ContentEquals, []string{"feed", "workers"})
}

// An HCL export of a read of one directory leaves coordination nodes not
// described, and applied back over a second directory it removes none of
// that directory's nodes.
func TestHostReappliesAnHCLExportOfANarrowedReadSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	events := path.Join(database, directory, "events")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+events+"`")
	createNode(c, opened, path.Join(database, directory, "local"))
	createNode(c, opened, path.Join(database, outside, "workers"))

	document, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")

	c.Assert(document, qt.Matches, `(?s).*ptah:not-described coordination_node.*`, qt.Commentf("%s", document))
	c.Assert(document, qt.Matches, `(?s).*coordination_node "local" \{.*`, qt.Commentf("%s", document))
	output, err := hostApplySelected(c, directory+","+outside, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", document, output))
	c.Assert(entries(c, opened, database, outside), qt.ContentEquals, []string{"workers"})
}
