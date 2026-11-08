//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's TestRenderForDialect_Replications: an HCL export
// reports the async replication it cannot declare, and applying the export
// back leaves the held replication and its replica table alone.
func TestHostHCLExportKeepsAnAsyncReplication_HappyPath(t *testing.T) {
	t.Setenv("PTAH_LOG_LEVEL", "debug")
	c := testcheck.New(t)
	_, session := accessSession(c)
	skipWithout(c, session, replication.CapabilityReplication)
	opened, directory, database := inspectionFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	source := path.Join(database, outside, "source")
	mirror := path.Join(database, directory, "mirror")
	replica := path.Join(database, directory, "replica")
	createSchemaObject(c, opened, "CREATE TABLE `"+source+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+source+"`")
	createSchemaObject(c, opened, "CREATE ASYNC REPLICATION `"+mirror+"` FOR `"+source+"` AS `"+replica+
		"` WITH (CONNECTION_STRING = '"+selfConnection(c, opened)+"')", "DROP ASYNC REPLICATION `"+mirror+"` CASCADE")

	document, reported := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(strings.ToLower(reported), qt.Contains, "feature object "+schema.ReplicationProperties+" "+directory+".mirror",
		qt.Commentf("%s\n%s", document, reported))
	c.Assert(strings.ToLower(reported), qt.Contains, "is not represented in hcl")
	c.Assert(hostPlanned(c, directory, "hcl", document), qt.HasLen, 0, qt.Commentf("%s\n%s", document, reported))
}

// The transfer half of TestRenderForDialect_Replications uses the same
// contract: HCL names no transfer, and its exported coverage keeps the held
// transfer when the document is applied back.
func TestHostHCLExportKeepsATransfer_HappyPath(t *testing.T) {
	t.Setenv("PTAH_LOG_LEVEL", "debug")
	c := testcheck.New(t)
	_, session := accessSession(c)
	skipWithout(c, session, replication.CapabilityTransfers)
	opened, directory, database := inspectionFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	topic := path.Join(database, outside, "events")
	target := path.Join(database, outside, "target")
	transfer := path.Join(database, directory, "copy")
	createSchemaObject(c, opened, "CREATE TABLE `"+target+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+target+"`")
	createSchemaObject(c, opened, "CREATE TOPIC `"+topic+"`", "DROP TOPIC `"+topic+"`")
	createSchemaObject(c, opened, "CREATE TRANSFER `"+transfer+"` FROM `"+topic+"` TO `"+target+
		"` USING ($m) -> { return []; }", "DROP TRANSFER `"+transfer+"`")

	document, reported := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(strings.ToLower(reported), qt.Contains, "feature object "+schema.TransferProperties+" "+directory+".copy",
		qt.Commentf("%s\n%s", document, reported))
	c.Assert(strings.ToLower(reported), qt.Contains, "is not represented in hcl")
	c.Assert(hostPlanned(c, directory, "hcl", document), qt.HasLen, 0, qt.Commentf("%s\n%s", document, reported))
}
