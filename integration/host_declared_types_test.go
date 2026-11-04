//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// heldColumnTypes reads the type of each column a table holds back from the
// table service, by name, as YDB prints it.
func heldColumnTypes(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	var described options.Description
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, s table.Session) error {
		var err error
		described, err = s.DescribeTable(ctx, absolute)
		return err
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	result := make(map[string]string, len(described.Columns))
	for _, column := range described.Columns {
		result[column.Name] = column.Type.Yql()
	}
	return result
}

// The host's Go annotations declare types with a detail YDB's types do not
// keep: the length of a VARCHAR and a CHAR, and the display width of an INT.
// Through this provider the table is created with the types that hold the
// values, as the built-in engine creates it, so a NOT NULL VARCHAR(80) with a
// default is a Utf8, and a second comparison finds nothing to change.
func TestHostCreatesATypeWhoseDetailYDBDoesNotKeep_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	source := "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n" +
		"\t//ptah:schema:field name=\"title\" type=\"VARCHAR(80)\" not_null default=\"untitled\"\n\tTitle string\n" +
		"\t//ptah:schema:field name=\"code\" type=\"CHAR(2)\"\n\tCode string\n" +
		"\t//ptah:schema:field name=\"n\" type=\"INT(11)\"\n\tN int32\n" +
		"\t//ptah:schema:index name=\"idx_items_title\" fields=\"title\"\n\t_ int\n}\n"

	applied, applyErr := hostApplySource(c, directory, "go", source)
	compared, compareErr := hostCompare(c, directory, source)

	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("%s", compared))
	c.Assert(compared, qt.Contains, "No schema differences detected")
	c.Assert(heldColumnTypes(c, opened, absolute), qt.DeepEquals, map[string]string{"id": "Int64", "title": "Utf8", "code": "Optional<Utf8>", "n": "Optional<Int32>"})
	c.Assert(heldIndexKinds(c, opened, absolute), qt.DeepEquals, map[string]options.IndexType{"idx_items_title": options.IndexTypeGlobal})
}
