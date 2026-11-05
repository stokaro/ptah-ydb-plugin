//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// migrationEntities declares the items table of directory with generic SQL
// types, and the obsolete table when it is given.
func migrationEntities(directory string, before bool) string {
	source := "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n" +
		"\t//ptah:schema:field name=\"name\" type=\"TEXT\"\n\tName string\n" +
		"\t//ptah:schema:field name=\"sku\" type=\"VARCHAR(32)\"\n\tSKU string\n"
	if before {
		return source + "\t//ptah:schema:field name=\"legacy\" type=\"TEXT\"\n\tLegacy string\n" +
			"\t//ptah:schema:index name=\"idx_items_sku\" fields=\"sku\"\n\t_ int\n" +
			"\t//ptah:schema:index name=\"idx_items_legacy\" fields=\"legacy\"\n\t__ int\n}\n\n" +
			"//ptah:schema:table name=\"obsolete\" schema=\"" + directory + "\"\ntype Obsolete struct {\n" +
			"\t//ptah:schema:field name=\"id\" type=\"INTEGER\" primary\n\tID int32\n}\n"
	}
	return source + "\t//ptah:schema:field name=\"stock\" type=\"INTEGER\"\n\tStock int32\n" +
		"\t//ptah:schema:index name=\"idx_items_name\" fields=\"name\"\n\t_ int\n}\n"
}

// One host plan over a table that holds rows, from a declaration in generic
// SQL types: it adds a column, drops a column an index covers, adds an index,
// drops another, and drops a table. YDB refuses to drop an indexed column, so
// the plan drops the index first. Nothing is left to plan, and the rows stay.
func TestHostPlansAMigrationAgainstTheLiveDatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"items", "obsolete"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute(table)+"`"), qt.IsNil)
		}
	})
	created, err := hostApplySource(c, directory, "go", migrationEntities(directory, true))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute("items")+"` (`id`, `name`, `sku`, `legacy`) "+
		"VALUES (1l, 'one'u, 'A-1'u, 'x'u), (2l, 'two'u, 'B-2'u, 'y'u)"), qt.IsNil)
	after := migrationEntities(directory, false)

	migrated, migrateErr := hostApplySource(c, directory, "go", after)
	compared, compareErr := hostCompare(c, directory, after)

	c.Assert(migrateErr, qt.IsNil, qt.Commentf("%s", migrated))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("%s", compared))
	c.Assert(compared, qt.Contains, "No schema differences detected")
	c.Assert(entryNames(heldEntries(c, opened, path.Join(database, directory))), qt.DeepEquals, []string{"items"})
	c.Assert(heldColumnTypes(c, opened, absolute("items")), qt.DeepEquals, map[string]string{
		"id": "Int64", "name": "Optional<Utf8>", "sku": "Optional<Utf8>", "stock": "Optional<Int32>"})
	c.Assert(heldIndexKinds(c, opened, absolute("items")), qt.DeepEquals, map[string]options.IndexType{"idx_items_name": options.IndexTypeGlobal})
	var rows uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute("items")+"` WHERE `stock` IS NULL").Scan(&rows), qt.IsNil)
	c.Assert(rows, qt.Equals, uint64(2))
}
