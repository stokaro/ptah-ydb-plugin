//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBDesiredYQL_Comments: YQL comments on a table,
// a column, an inline global index and a view are created with them in one
// run, changed to text with a semicolon and a newline in it, and removed, and
// each run leaves nothing to plan. The host's test runs in a dev realm; here
// the schema names the fixture directory.
func TestHostAppliesYQLComments_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	replacer := strings.NewReplacer("{dir}", directory)
	declarations := replacer.Replace("CREATE TABLE `{dir}/items` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id), INDEX by_body GLOBAL ON (body)); " +
		"CREATE VIEW `{dir}/summary` WITH (security_invoker = TRUE) AS SELECT id FROM `{dir}/items`;\n")
	items, summary := path.Join(database, directory, "items"), path.Join(database, directory, "summary")

	for _, step := range []struct {
		name, comments string
		table, view    map[string]string
	}{
		{name: "set", comments: "COMMENT ON TABLE `{dir}/items` IS 'table'; COMMENT ON COLUMN `{dir}/items`.body IS 'column'; " +
			"COMMENT ON INDEX by_body ON `{dir}/items` IS 'index'; COMMENT ON VIEW `{dir}/summary` IS 'view';",
			table: map[string]string{"ptah.comment": "table", "ptah.comment.column.body": "column", "ptah.comment.index.by_body": "index"},
			view:  map[string]string{"ptah.comment": "view"}},
		{name: "changed", comments: "COMMENT ON TABLE `{dir}/items` IS 'changed; table'; COMMENT ON COLUMN `{dir}/items`.body IS 'line\\nnext'; " +
			"COMMENT ON INDEX by_body ON `{dir}/items` IS 'changed index'; COMMENT ON VIEW `{dir}/summary` IS 'changed view';",
			table: map[string]string{"ptah.comment": "changed; table", "ptah.comment.column.body": "line\nnext", "ptah.comment.index.by_body": "changed index"},
			view:  map[string]string{"ptah.comment": "changed view"}},
		{name: "removed", table: make(map[string]string), view: make(map[string]string)},
	} {
		applyRuns(c, directory, "sql", declarations+replacer.Replace(step.comments))
		c.Assert(storedAttributes(c, opened, items), qt.DeepEquals, step.table, qt.Commentf("%s", step.name))
		c.Assert(storedAttributes(c, opened, summary), qt.DeepEquals, step.view, qt.Commentf("%s", step.name))
	}
}

// Ported from the host's TestYDBDesiredYQL_ExternalObjects: a YQL data source
// and an external table over it are created, then replaced by CREATE OR
// REPLACE with the source moved, a column added and a space for a delimiter,
// and then removed, with the server replacing an external object in place
// and with it doing so by a drop and a creation. Each run leaves nothing to
// plan, and the delimiter reads back as declared. The host's test runs in a
// dev realm; here the schema names the fixture directory. The flag a mode
// turns on is restored when it ends.
func TestHostAppliesYQLExternalObjects_HappyPath(t *testing.T) {
	for _, mode := range []struct {
		name    string
		replace bool
	}{
		{name: "recreate"},
		{name: "replace", replace: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			newClusterFlags(c, opened).set(c, map[string]bool{"EnableExternalDataSources": true, "EnableReplaceIfExistsForExternalEntities": mode.replace})
			emptyAtCleanup(c, opened, path.Join(database, directory))
			replacer := strings.NewReplacer("{dir}", directory)
			for _, step := range []struct{ name, source, delimiter string }{
				{name: "created", delimiter: ";", source: "CREATE EXTERNAL DATA SOURCE `{dir}/bucket` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.invalid/old/', AUTH_METHOD = 'NONE'); " +
					"CREATE EXTERNAL TABLE `{dir}/events` (id Int64 NOT NULL, amount Decimal(22,9)) WITH (DATA_SOURCE = '{dir}/bucket', LOCATION = '/', FORMAT = 'csv_with_names', CSV_DELIMITER = ';');"},
				{name: "replaced", delimiter: " ", source: "CREATE OR REPLACE EXTERNAL DATA SOURCE `{dir}/bucket` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.invalid/new/', AUTH_METHOD = 'NONE'); " +
					"CREATE OR REPLACE EXTERNAL TABLE `{dir}/events` (id Int64 NOT NULL, amount Decimal(22,9), body Utf8) WITH (DATA_SOURCE = '{dir}/bucket', LOCATION = '/', FORMAT = 'csv_with_names', CSV_DELIMITER = ' ');"},
			} {
				applyRuns(c, directory, "sql", replacer.Replace(step.source))
				c.Assert(describeExternalTable(c, opened, path.Join(database, directory, "events")).Content["CSV_DELIMITER"], qt.Equals, `["`+step.delimiter+`"]`, qt.Commentf("%s", step.name))
			}
			c.Assert(describeSource(c, opened, path.Join(database, directory, "bucket")).Location, qt.Equals, "https://storage.invalid/new/")
			c.Assert(describeExternalTable(c, opened, path.Join(database, directory, "events")).Columns, qt.DeepEquals, []string{"id", "amount", "body"})
			applyRuns(c, directory, "sql", "\n")
			c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.HasLen, 0)
		})
	}
}
