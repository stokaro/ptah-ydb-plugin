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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests here export a read of one directory through the host's own
// writers, `ptah introspect` and `ptah schema inspect --format hcl`, and apply
// the export back, as the host's embedded YDB engine is measured to: the
// export is complete for the selection it is applied to. Applied over the
// directory it was read from, it plans nothing, and an object whose
// declaration is removed from it is dropped; applied over that directory and
// a second one, it drops the second directory's objects too.

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

// withoutLines is text without the lines that contain any of marks.
func withoutLines(text string, marks ...string) string {
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if !slices.ContainsFunc(marks, func(mark string) bool { return strings.Contains(line, mark) }) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// introspectDirectory writes a read of directory as a Go file, as `ptah
// introspect --schemas` writes it.
func introspectDirectory(c *qt.C, directory string) string {
	c.Helper()
	out := c.TempDir()
	hostExport(c, "introspect", "--schemas", directory, "--out", out, "--single-file")
	written, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	return string(written)
}

// A Go export of a read of one directory says nothing of the topics it read
// less, applied back over that directory plans nothing, and without the
// topic's directive drops the topic.
func TestHostAppliesAGoExportOfADirectoryAsComplete(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	events, stream := path.Join(database, directory, "events"), path.Join(database, directory, "stream")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+events+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+stream+"` (CONSUMER reader)", "DROP TOPIC `"+stream+"`")

	source := introspectDirectory(c, directory)

	c.Assert(source, qt.Contains, `//ptah:schema:topic name="stream" schema="`+directory+`"`, qt.Commentf("%s", source))
	c.Assert(source, qt.Not(qt.Contains), `kind="topic"`, qt.Commentf("%s", source))
	output, err := hostApplySelected(c, directory, "go", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", source, output))
	withoutTopic := withoutLines(source, "ptah:schema:topic")
	output, err = hostReplanSource(c, directory, "go", withoutTopic)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", withoutTopic, output))
	c.Assert(output, qt.Contains, directory+".stream: DROP TOPIC", qt.Commentf("%s\n%s", withoutTopic, output))
}

// A Go export of a read of one directory, applied over that directory and a
// second one, drops the second directory's topic and coordination node, as
// the embedded engine's export does: the document describes every directory
// the apply names.
func TestHostAppliesAGoExportOfADirectoryOverAnother(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	events, stream, feed := path.Join(database, directory, "events"), path.Join(database, directory, "stream"), path.Join(database, outside, "feed")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+events+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+stream+"` (CONSUMER reader)", "DROP TOPIC `"+stream+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+feed+"` (CONSUMER reader)", "DROP TOPIC `"+feed+"`")
	createNode(c, opened, path.Join(database, outside, "workers"))

	source := introspectDirectory(c, directory)

	output, err := hostReplanSource(c, directory+","+outside, "go", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Contains, outside+".feed: DROP TOPIC", qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Contains, outside+".workers: Drops the node", qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Not(qt.Contains), directory+".stream", qt.Commentf("%s\n%s", source, output))
}

// An HCL export of a read of one directory says nothing of the coordination
// nodes it read less, applied back over that directory plans nothing, and
// without a node's block drops the node; applied over a second directory too,
// it drops that directory's node. Ported from the export half of the host's
// TestYDBCoordinationNodes_HCLRequiresExplicitNamespaceClaim: the export
// without the node's block, applied, drops the node, and the next plan over
// it is empty.
func TestHostAppliesAnHCLExportOfADirectoryAsComplete(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	events, local := path.Join(database, directory, "events"), path.Join(database, directory, "local")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+events+"`")
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), local, coordination.NodeConfig{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 10000}), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// The test drops the node; a run that fails before the drop leaves it.
		_ = opened.Driver.Coordination().DropNode(cleanup, local)
	})
	createNode(c, opened, path.Join(database, outside, "workers"))

	document, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")

	c.Assert(document, qt.Not(qt.Contains), "ptah:not-described coordination_node", qt.Commentf("%s", document))
	c.Assert(document, qt.Matches, `(?s).*coordination_node "local" \{.*`, qt.Commentf("%s", document))
	output, err := hostApplySelected(c, directory, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", document, output))
	withoutNode := regexp.MustCompile(`(?s)coordination_node "local" \{.*?\n\}\n`).ReplaceAllString(document, "")
	output, err = hostReplanSource(c, directory, "hcl", withoutNode)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", withoutNode, output))
	c.Assert(output, qt.Contains, directory+".local: Drops the node", qt.Commentf("%s\n%s", withoutNode, output))
	output, err = hostReplanSource(c, directory+","+outside, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Contains, outside+".workers: Drops the node", qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Not(qt.Contains), directory+".local", qt.Commentf("%s\n%s", document, output))
	output, err = hostApplySelected(c, directory, "hcl", withoutNode)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", withoutNode, output))
	_, found := describedNode(c, opened, database, directory, "local")
	c.Assert(found, qt.IsFalse)
	c.Assert(hostPlanned(c, directory, "hcl", withoutNode), qt.HasLen, 0)
}
