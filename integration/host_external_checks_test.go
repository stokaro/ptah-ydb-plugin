//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// externalRows creates, in the fixture directory, an object storage data
// source at a location no server answers, an external table files over it
// and a row table rows, and drops them when the test ends.
func externalRows(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	source, files, rows := path.Join(database, directory, "source"), path.Join(database, directory, "files"), path.Join(database, directory, "rows")
	createSchemaObject(c, opened, "CREATE EXTERNAL DATA SOURCE `"+source+"` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.invalid/b/', AUTH_METHOD = 'NONE')",
		"DROP EXTERNAL DATA SOURCE `"+source+"`")
	createSchemaObject(c, opened, "CREATE EXTERNAL TABLE `"+files+"` (id Int64) WITH (DATA_SOURCE = '"+source+"', LOCATION = 'f/', FORMAT = 'json_each_row')",
		"DROP EXTERNAL TABLE `"+files+"`")
	createSchemaObject(c, opened, "CREATE TABLE `"+rows+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+rows+"`")
}

// Ported from the db verify half of the host's
// TestYDBChecks_RefuseAReadOfAnExternalTable: a check that reads an external
// table, whose rows the server would fetch from the data source's location,
// is refused before it runs, and the run exits with the status a fault gets.
// A check over a row table beside it is the control and is verified.
func TestHostVerifiesNoCheckOverAnExternalTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	externalRows(c, opened, database, directory)
	checks := writeFiles(c, c.TempDir(), map[string]string{"external.sql": "-- +ptah check name=\"external\" assert=\"SELECT COUNT(*) = 0 FROM `" + directory + "/files`\"\n" +
		"-- +ptah check name=\"rows\" assert=\"SELECT COUNT(*) = 0 FROM `" + directory + "/rows`\"\n"})

	reported, err := hostCommand(c, "db", "verify", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--checks", filepath.Join(checks, "external.sql"))

	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(reported, qt.Contains, "Verdict: errored (1 verified, 0 failed, 1 errored of 2)")
	c.Assert(reported, qt.Contains, "/"+directory+"/files is an external table, whose rows the server fetches from its data source")
}

// Ported from the migration half of the host's
// TestYDBChecks_RefuseAReadOfAnExternalTable: a migration whose check reads
// an external table fails on the check, named, before its body runs, so the
// table the body creates is not made.
func TestHostRefusesAMigrationCheckOverAnExternalTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	externalRows(c, opened, database, directory)
	never := path.Join(directory, "never")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// The table exists only when the check failed to stop the body.
		_ = execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+path.Join(database, never)+"`")
		emptyTree(ctx, opened, path.Join(database, directory, "m"))
		_ = opened.Driver.Scheme().RemoveDirectory(ctx, path.Join(database, directory, "m"))
	})
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_guarded.up.sql": "-- +ptah check name=\"no_files\" assert=\"SELECT COUNT(*) = 0 FROM `" + directory + "/files`\"\n" +
			"CREATE TABLE `" + never + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_guarded.down.sql": "DROP TABLE `" + never + "`;\n",
	})

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "migrations", "up", "--migrations-dir", migrations, "--migrations-schema", path.Join(directory, "m"))

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "no_files", qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "is an external table, whose rows the server fetches from its data source", qt.Commentf("%s", output))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.Not(qt.Contains), "never")
}
