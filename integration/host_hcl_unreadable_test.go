//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topicoptions"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hostExportDebug runs `ptah schema inspect --format hcl` over directory with
// this provider selected for ydb and debug logging on, which lists each
// detail the document leaves out, and returns the document and what the host
// reported.
func hostExportDebug(c *qt.C, directory string) (document, reported string) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "inspect", "--schemas", directory, "--format", "hcl", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"))
	cmd.Env = append(os.Environ(), "PTAH_LOG_LEVEL=debug")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s\n%s", stdout.String(), stderr.String()))
	return stdout.String(), stderr.String()
}

// unreadableConsumer adds to the topic at absolute a consumer whose codec list
// names codec 10042, which no declaration names: YDB keeps the number on
// 25.1.4.7 and 26.2.1.14, and a read cannot describe the topic with it.
func unreadableConsumer(c *qt.C, opened *connection.Connection, absolute string) {
	c.Helper()
	c.Assert(opened.Driver.Topic().Alter(c.Context(), absolute,
		topicoptions.AlterWithAddConsumers(topictypes.Consumer{Name: "custom", SupportedCodecs: []topictypes.Codec{10042}})), qt.IsNil)
}

// An HCL export of a directory whose table has a changefeed with a consumer
// the read cannot describe writes the table and leaves the changefeed out.
// Its coverage header records the read limit. Applied back, the document
// plans nothing and the changefeed keeps its consumer. Ported from the host's
// TestSchemaInspectHCLKeepsAnOwnersLimitInTheHeader (internal/atlasreport),
// whose record of such a changefeed the built-in YDB engine wrote from a read
// it was handed.
func TestHostHCLExportLeavesOutAChangefeedTheReadCannotDescribe_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	table := path.Join(database, directory, "t")
	createSchemaObject(c, opened, "CREATE TABLE `"+table+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+table+"`")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+table+"` ADD CHANGEFEED updates WITH (MODE = 'UPDATES', FORMAT = 'JSON')"), qt.IsNil)
	unreadableConsumer(c, opened, table+"/updates")

	document, reported := hostExportDebug(c, directory)
	planned := hostPlanned(c, directory, "hcl", document)
	feeds, found := describedChangefeeds(c, opened, table)

	c.Assert(document, qt.Contains, "table \"t\" {\n  schema = schema."+directory+"\n")
	c.Assert(document, qt.Not(qt.Contains), "ptah-ydb-plugin/changefeed\"")
	c.Assert(document, qt.Contains, `"kind":"github.com/stokaro/ptah-ydb-plugin/table-changefeeds","subject"`)
	c.Assert(document, qt.Contains, `"state":"unrepresentable","reason":"changefeed \"updates\" has settings this provider cannot represent`)
	c.Assert(document, qt.Contains, `consumer \"custom\": its codec list names codec 10042, which Ptah does not declare`)
	c.Assert(reported, qt.Not(qt.Contains), "changefeed updates is not represented in HCL")
	c.Assert(planned, qt.HasLen, 0)
	c.Assert(found, qt.IsTrue)
	c.Assert(feeds["updates"].Consumers, qt.DeepEquals, []string{"custom"})
}

// An HCL export of a directory holding a topic with a consumer the read
// cannot describe leaves the topic out, and the host lists it among the
// details HCL cannot represent. A plan over the directory is refused by the
// topic's path and why, as the built-in YDB engine at host 74de0b65b refused
// to read it (`YDB topic /local/<dir>/legacy: consumer "custom": its codec list
// names codec 10042, which Ptah does not declare`), whether the source makes
// no claim on topics, as the export does, declares the topic or drops it, and
// the topic stays. Without the reason the host prints `provider compare
// models failed (InvalidArgument)` alone (G241). Ported from the topic half of
// the host's TestRender_ReportsTheTopicsItCannotRecord (internal/atlashclrender).
func TestHostRefusesAPlanOverATopicTheReadCannotDescribe_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	topic := path.Join(database, directory, "legacy")
	createSchemaObject(c, opened, "CREATE TOPIC `"+topic+"` (CONSUMER worker)", "DROP TOPIC `"+topic+"`")
	unreadableConsumer(c, opened, topic)
	document, reported := hostExportDebug(c, directory)
	files := writeFiles(c, c.TempDir(), map[string]string{"export.hcl": document,
		"declared.yaml": "topics:\n  legacy:\n    schema: " + directory + "\n    consumers:\n      worker: {}\n",
		"dropped.yaml":  "topics: {}\n"})
	const why = `: consumer "custom": its codec list names codec 10042, which Ptah does not declare`

	c.Assert(document, qt.Not(qt.Contains), "ptah-ydb-plugin/topic\"")
	c.Assert(document, qt.Not(qt.Contains), "legacy")
	c.Assert(reported, qt.Contains, "github.com/stokaro/ptah-ydb-plugin/topic "+directory+".legacy of kind github.com/stokaro/ptah-ydb-plugin/topic is not represented in HCL")
	for _, source := range []string{"export.hcl", "declared.yaml", "dropped.yaml"} {
		t.Run(source, func(t *testing.T) {
			c := testcheck.New(t)

			refused, err := hostCommand(c, "schema", "apply", "--dry-run", "--tx-mode", "none", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory,
				"--schema-file", filepath.Join(files, source))

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
			c.Assert(refused, qt.Contains, "error: compare database schema: provider compare models failed (InvalidArgument): YDB topic "+directory+"/legacy"+why)
			c.Assert(childNames(c, opened, path.Join(database, directory)), qt.Contains, "legacy")
		})
	}
}
