//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// compatStatementFiles are two migrations of a table in directory: the table,
// then rows written by a statement whose @@...@@ string holds a semicolon, one
// whose double-quoted string holds one, and a DO BEGIN block of two statements.
func compatStatementFiles(directory string) map[string]string {
	table := "`" + directory + "/s`"
	return map[string]string{
		"20260101000001_s.sql": "CREATE TABLE " + table + " (id Int64 NOT NULL, s Utf8, PRIMARY KEY (id));\n",
		"20260101000002_rows.sql": "UPSERT INTO " + table + " (id, s) VALUES (1, @@a;b@@);\n" +
			"UPSERT INTO " + table + " (id, s) VALUES (2, \"c;d\");\n" +
			"DO BEGIN UPSERT INTO " + table + " (id, s) VALUES (3, 'e'); UPSERT INTO " + table + " (id, s) VALUES (4, 'f'); END DO;\n",
	}
}

// `ptah-compat migrate apply --format '{{ json . }}'` reports the statements
// each file ran, cut where a statement of YQL ends, as the built-in YDB engine
// at host 74de0b65b reported them on 25.1.4.7 and 26.2.1.14: a semicolon
// inside a string or a DO BEGIN block ends none. Read with the shared rules,
// the report listed `... VALUES (1, @@a` and `b@@)` as two statements, and the
// block as three. The migrations run in a directory of the database rather
// than in a dev realm, which refuses a DO BEGIN block whose effects it cannot
// confine.
func TestHostCompatMigrateApplyReportsTheStatementsOfAFile_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"s", "atlas_schema_revisions"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	dir := c.TempDir()
	for name, body := range compatStatementFiles(directory) {
		compatFile(c, dir, name, body)
	}
	address := "file://" + filepath.ToSlash(dir)
	table := "`" + directory + "/s`"

	_, hashNotes, hashExit := runCompat(c, nil, "migrate", "hash", "--dir", address)
	reported, applyNotes, applyExit := runCompat(c, nil, "migrate", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--dir", address,
		"--revisions-schema", directory, "--format", "{{ json . }}")

	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s", hashNotes))
	c.Assert(applyExit, qt.Equals, 0, qt.Commentf("migrate apply:\n%s\n%s", reported, applyNotes))
	var report struct {
		Applied []struct {
			Name    string
			Applied []string
		}
	}
	c.Assert(json.Unmarshal([]byte(reported), &report), qt.IsNil, qt.Commentf("%s", reported))
	c.Assert(report.Applied, qt.HasLen, 2)
	c.Assert(report.Applied[0].Applied, qt.DeepEquals, []string{"CREATE TABLE " + table + " (id Int64 NOT NULL, s Utf8, PRIMARY KEY (id))"})
	c.Assert(report.Applied[1].Applied, qt.DeepEquals, []string{
		"UPSERT INTO " + table + " (id, s) VALUES (1, @@a;b@@)",
		"UPSERT INTO " + table + " (id, s) VALUES (2, \"c;d\")",
		"DO BEGIN UPSERT INTO " + table + " (id, s) VALUES (3, 'e'); UPSERT INTO " + table + " (id, s) VALUES (4, 'f'); END DO",
	})
	c.Assert(rowCount(c, opened, path.Join(database, directory, "s"), ""), qt.Equals, int64(4))
}
