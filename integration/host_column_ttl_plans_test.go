//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// sourceDocument declares the fixture's data source at location and no table.
func (f tieredFixture) sourceDocument(location string) string {
	return strings.TrimSuffix(f.document(location, ""), f.tables("")) + "tables: {}\n"
}

// Ported from the host's
// TestGenerateMigrationAST_ColumnTableRemovedBeforeItsSourceIsRecreated
// (internal/planner/dialects/ydb): a column table whose TTL moves rows to a
// data source, removed from the document while the source moves to another
// location, is dropped before the source is replaced, since YDB keeps a data
// source a TTL names. The built-in engine at 74de0b65b planned DROP TABLE,
// then DROP and CREATE of the source, on 25.1.4.7 and 26.2.1.14; this
// provider replaces the source in one CREATE OR REPLACE after the drop. The
// source then stands at the new location and nothing is left to plan.
func TestHostDropsATieredTableBeforeItsSourceIsReplaced_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fixture := newTieredFixture(c)
	const first, replaced = "https://column.invalid/archive/", "https://column.invalid/replaced/"
	applyUntilSynced(c, fixture.directory, "yaml", fixture.document(first, tieredTTL))

	output, err := hostApplySelected(c, fixture.directory, "yaml", fixture.sourceDocument(replaced))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{"table-drop-operation " + fixture.directory + ".records",
		"external-data-source-operation " + fixture.directory + ".archive"}, qt.Commentf("%s", output))
	synced, err := hostApplySelected(c, fixture.directory, "yaml", fixture.sourceDocument(replaced))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", synced))
	c.Assert(synced, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(fixture.source(c), qt.Equals, replaced)
}

// Ported from the host's TestGenerateMigrationAST_ColumnTTLFromRowPolicy: a
// column table whose TTL only deletes takes a TTL that moves rows to a data
// source first. The built-in engine at 74de0b65b planned RESET (TTL) and then
// SET; YDB 25.1.4.7 and 26.2.1.14 replace a column table's TTL with the SET
// alone, which is what this provider plans. The table then holds the tiered
// policy and nothing is left to plan.
func TestHostReplacesADeletingColumnTTLWithATieredOne_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fixture := newTieredFixture(c)
	const location = "https://column.invalid/archive/"
	applyUntilSynced(c, fixture.directory, "yaml", fixture.document(location, deletingTTL))
	c.Assert(fixture.read(c), qt.Equals, "604800 delete")

	output, err := hostApplySelected(c, fixture.directory, "yaml", fixture.document(location, tieredTTL))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{"column-table-ttl-operation " + fixture.directory + ".records"}, qt.Commentf("%s", output))
	synced, err := hostApplySelected(c, fixture.directory, "yaml", fixture.document(location, tieredTTL))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", synced))
	c.Assert(synced, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(fixture.read(c), qt.Equals, "86400 "+fixture.archive()+", 604800 delete")
}
