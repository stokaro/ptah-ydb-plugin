//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// builtinGoExport is the table declarations the host's embedded engine wrote
// for the tables builtinExportStatements makes, recorded in the directory
// "ex" and read here as declarations of the tables at a dev realm's root;
// see testdata/builtin-go-export.
func builtinGoExport(c *qt.C) string {
	c.Helper()
	recorded, err := os.ReadFile("testdata/builtin-go-export/tables.txt")
	c.Assert(err, qt.IsNil)
	return strings.NewReplacer(` schema="ex"`, "", `table="ex.`, `table="`).Replace(string(recorded))
}

// builtinExportStatements create, in the directory at absolute, the tables of
// testdata/builtin-go-export whose declarations this provider writes: a row
// table with nothing of its own, one with a TTL, settings, a column family
// and an index's settings, and a column table.
func builtinExportStatements(absolute string) []string {
	return []string{
		"CREATE TABLE `" + absolute + "/plain` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))",
		"CREATE TABLE `" + absolute + "/tuned` (id Uint64 NOT NULL, body Utf8, created_at Timestamp, PRIMARY KEY (id), INDEX by_body GLOBAL ON (body), " +
			"FAMILY cold (COMPRESSION = \"lz4\")) WITH (TTL = Interval(\"P30D\") ON created_at, AUTO_PARTITIONING_BY_LOAD = ENABLED, " +
			"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, READ_REPLICAS_SETTINGS = \"ANY_AZ:1\", KEY_BLOOM_FILTER = ENABLED)",
		"ALTER TABLE `" + absolute + "/tuned` ALTER INDEX by_body SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)",
		"CREATE TABLE `" + absolute + "/cols` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id)) PARTITION BY HASH (id) " +
			"WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2)",
	}
}

// exportLine is the one line of text that starts with prefix.
func exportLine(c *qt.C, text, prefix string) string {
	c.Helper()
	var found []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	c.Assert(found, qt.HasLen, 1, qt.Commentf("lines starting with %q in:\n%s", prefix, text))
	return found[0]
}

// A Go export writes what the host's embedded engine wrote for the same
// tables: a column table's storage as store="column", a table's TTL before
// its settings, and the families a table was given but not the default family
// every row table holds with nothing of its own. Applied back, the export
// plans nothing. The tables sit at a dev realm's root, which keeps the
// database root's lock node out of the run.
func TestHostGoExportWritesWhatTheEmbeddedEngineWrites_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, _ := inspectionFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"plain", "tuned", "cols"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, name)+"`"), qt.IsNil)
		}
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(root, locking.NodeName))
	})
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), root), qt.IsNil)
	for _, statement := range builtinExportStatements(root) {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	recorded := builtinGoExport(c)
	tuned := `//ptah:schema:table name="tuned" `
	recordedTuned := exportLine(c, recorded, tuned)
	out := c.TempDir()

	exported, exportErr := hostCommand(c, "introspect", "--db-url", address, "--out", out, "--single-file")
	written, readErr := os.ReadFile(filepath.Join(out, "schema.go"))
	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", out, "--auto-approve", "--tx-mode", "none")

	c.Assert(exportErr, qt.IsNil, qt.Commentf("%s", exported))
	c.Assert(readErr, qt.IsNil)
	columnTable := `//ptah:schema:table name="cols" `
	c.Assert(exportLine(c, string(written), columnTable), qt.Equals, exportLine(c, recorded, columnTable))
	family := `//ptah:schema:columnfamily name="cold" `
	c.Assert(exportLine(c, string(written), family), qt.Equals, exportLine(c, recorded, family))
	c.Assert(string(written), qt.Not(qt.Contains), `//ptah:schema:columnfamily name="default"`)
	ttl := recordedTuned[:strings.Index(recordedTuned, ` auto_partitioning_`)]
	c.Assert(exportLine(c, string(written), tuned), qt.Matches, regexp.QuoteMeta(ttl)+` auto_partitioning_.*`)
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
}
