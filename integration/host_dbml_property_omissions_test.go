//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A live YDB read includes empty changefeed and index sets, default column
// properties, and the server's default row-table settings. None is a loss of
// information when DBML describes the plain table.
func TestHostInspectedPlainTableHasNoYDBPropertyWarnings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	plain := path.Join(database, directory, "plain")
	createSchemaObject(c, opened, "CREATE TABLE `"+plain+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+plain+"`")

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "dbml")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `"plain"`)
	c.Assert(output, qt.Not(qt.Contains), "DBML cannot express YDB")
}

// A common global index with YDB's default partitioning has no provider
// index setting to omit. The host writes the index through its common model.
func TestHostInspectedDefaultIndexHasNoYDBPropertyWarnings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	plain := path.Join(database, directory, "plain")
	createSchemaObject(c, opened, "CREATE TABLE `"+plain+"` (id Uint64 NOT NULL, label Utf8, PRIMARY KEY (id), INDEX by_label GLOBAL ON (label))",
		"DROP TABLE `"+plain+"`")

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "dbml")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `"by_label"`)
	c.Assert(output, qt.Not(qt.Contains), "DBML cannot express YDB")
}

// Nondefault TTL and family settings still appear as omissions, and selecting
// only the plain table removes the rich table's warnings.
func TestHostDBMLPropertyWarningsRespectTableSelection_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	events, plain := path.Join(database, directory, "events"), path.Join(database, directory, "plain")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, created_at Timestamp, payload Utf8 FAMILY cold, PRIMARY KEY (id), "+
		"FAMILY cold (COMPRESSION = 'lz4')) WITH (TTL = Interval('P1D') ON created_at)", "DROP TABLE `"+events+"`")
	createSchemaObject(c, opened, "CREATE TABLE `"+plain+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+plain+"`")
	url := os.Getenv("PTAH_YDB_TEST_URL")

	all, err := hostRunAt(c, url, "schema", "inspect", "--schemas", directory, "--format", "dbml")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", all))
	c.Assert(all, qt.Contains, "DBML cannot express YDB row-table properties (1)")
	c.Assert(all, qt.Contains, "DBML cannot express YDB column properties (1)")
	c.Assert(all, qt.Not(qt.Contains), "DBML cannot express YDB index sets")
	c.Assert(all, qt.Not(qt.Contains), "DBML cannot express changefeeds")

	selected, err := hostRunAt(c, url, "schema", "inspect", "--schemas", directory, "--include", directory+".plain", "--format", "dbml")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", selected))
	c.Assert(selected, qt.Contains, `"plain"`)
	c.Assert(selected, qt.Not(qt.Contains), `"events"`)
	c.Assert(selected, qt.Not(qt.Contains), "DBML cannot express YDB")
}
