//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// itemsWithN declares table items of directory with a column n declared as
// column, such as `{type: Int32}`.
func itemsWithN(directory, column string) string {
	return "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      n: " + column + "\n"
}

// notNullItems declares items with n NOT NULL, and nullableItems with n
// taking NULL.
func notNullItems(directory string) string {
	return itemsWithN(directory, "{type: Int32, not_null: true}")
}
func nullableItems(directory string) string { return itemsWithN(directory, "{type: Int32}") }

// A NOT NULL column the declaration lets take NULL is changed in place with
// ALTER COLUMN ... DROP NOT NULL, as the host's embedded engine planned it,
// on 25.1.4.7 and 26.2.1.14: the table keeps its rows, the column takes NULL,
// and nothing is left to plan. The plugin refused the change (`Column "n"
// definition change requires an owned column operation`).
func TestHostDropsNotNullInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", notNullItems(directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n) VALUES (1ul, 7)"), qt.IsNil)

	planned := hostPlanned(c, directory, "yaml", nullableItems(directory))
	applyRuns(c, directory, "yaml", nullableItems(directory))

	c.Assert(planned, qt.DeepEquals, []string{"column-nullability-operation " + directory + ".items"})
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int32>")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n) VALUES (2ul, NULL)"), qt.IsNil)
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
}

// A migration file drops the NOT NULL in place, and its down file, which
// makes the column NOT NULL again, rebuilds the table, as the host's embedded
// engine wrote both; both run and the declaration plans nothing after each.
func TestHostDropsNotNullThroughAMigration_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", notNullItems(directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n) VALUES (1ul, 7)"), qt.IsNil)

	migrations, up, down := hostGenerate(c, directory, "yaml", nullableItems(directory), "--allow-table-rebuild")
	// The host rates DROP NOT NULL destructive (DS104), whichever engine
	// plans it.
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up", "--allow-destructive")
	settled := hostPlanned(c, directory, "yaml", nullableItems(directory))
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER TABLE `"+directory+"/items` ALTER COLUMN `n` DROP NOT NULL;")
	c.Assert(down, qt.Contains, "CREATE TABLE `"+directory+"/__ptah_rebuild_items`")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Int32")
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
	c.Assert(hostPlanned(c, directory, "yaml", notNullItems(directory)), qt.HasLen, 0)
}
