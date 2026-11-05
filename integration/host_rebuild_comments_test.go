//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// countedItems is table items of the fixture directory in YAML, its count
// column of the type given, with the table's and the columns' comments where
// commented is set.
func countedItems(directory, count string, commented bool) string {
	table, key, counted := "", "", ""
	if commented {
		table, key, counted = "    comment: \"Items\"\n", ", comment: \"Key\"", ", comment: \"Count\""
	}
	return "tables:\n  items:\n    schema: " + directory + "\n" + table + "    columns:\n" +
		"      id: {type: Uint64, primary: true, not_null: true" + key + "}\n" +
		"      n: {type: " + count + counted + "}\n"
}

// A rebuild keeps the comments of the table and of its columns: the new
// table is created with the attributes YDB keeps them in, the rows are
// copied, and nothing is left to plan. Gap G114 of stokaro/ptah#4204: the
// provider read the comments from the planning request's observed common
// objects, which a host fills for no table, so a rebuilt table had none and
// the next run set them again.
//
// The comments are set by a change after the creation, since the provider
// path of the released host leaves the comments of a created table out
// (stokaro/ptah#5256).
func TestHostRebuildKeepsTheTableAndColumnComments_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", countedItems(directory, "Int32", false))
	applyRuns(c, directory, "yaml", countedItems(directory, "Int32", true))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n) VALUES (1ul, 7)"), qt.IsNil)

	applyUntilSynced(c, directory, "yaml", countedItems(directory, "Int64", true), "--allow-table-rebuild")

	c.Assert(storedAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment": "Items", "ptah.comment.column.id": "Key", "ptah.comment.column.n": "Count"})
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(rowCount(c, opened, absolute, "n = 7l"), qt.Equals, int64(1))
}
