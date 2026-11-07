//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// An inspected empty Utf8 or String default is present, while a column with
// no default remains absent. The host writes both its YQL and DBML projection
// from the same live provider read. Ported from the YDB case of
// TestConvertDBSchemaToGoSchema_YDBEmptyDefaultsSurviveExport.
func TestHostInspectedEmptyDefaultsSurviveSQLAndDBMLExport_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	items := path.Join(database, directory, "items")
	createSchemaObject(c, opened, "CREATE TABLE `"+items+"` (id Int64 NOT NULL, text_value Utf8 DEFAULT ''u, "+
		"bytes_value String DEFAULT '', no_default Utf8, PRIMARY KEY (id))", "DROP TABLE `"+items+"`")
	url := os.Getenv("PTAH_YDB_TEST_URL")

	sql, err := hostRunAt(c, url, "schema", "inspect", "--schemas", directory, "--format", "sql")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", sql))
	c.Assert(sql, qt.Contains, "`text_value` Utf8 DEFAULT ''u")
	c.Assert(sql, qt.Contains, "`bytes_value` String DEFAULT ''")
	c.Assert(sql, qt.Not(qt.Contains), "`no_default` Utf8 DEFAULT")

	dbml, err := hostRunAt(c, url, "schema", "inspect", "--schemas", directory, "--format", "dbml")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", dbml))
	c.Assert(dbml, qt.Contains, `"text_value" Utf8 [default: '']`)
	c.Assert(dbml, qt.Contains, `"bytes_value" String [default: '']`)
	c.Assert(dbml, qt.Not(qt.Contains), `"no_default" Utf8 [default:`)
}
