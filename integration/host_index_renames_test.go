//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// commentedItems declares items in directory with a synchronous index on kind
// named index, whose comment is By kind.
func commentedItems(directory, index string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"kind\" type=\"TEXT\"\n\t//ptah:schema:index name=\"" + index + "\" fields=\"kind\" comment=\"By kind\"\n\tKind string\n}\n"
}

// describedItems reads items back from the table service: its indexes by
// name and its user attributes, where YDB keeps Ptah's comments.
func describedItems(c *qt.C, opened *connection.Connection, absolute string) (map[string]*native.TableIndexDescription, map[string]string) {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	indexes := map[string]*native.TableIndexDescription{}
	for _, index := range described.GetIndexes() {
		indexes[index.GetName()] = index
	}
	return indexes, described.GetAttributes()
}

// The host's TestYDBGlobalIndexes_RenameRoundTrip. With EnableMoveIndex on, a
// session reports index_rename to a host that lists planning.index-renames,
// and the index the declaration renames is one step that renames it in place,
// not a removal and an addition. The index keeps its key, the partitioning an
// operator gave it, its comment and the reads through it, and the next run is
// synced.
func TestHostRenamesAnIndexInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": true})
	output, err := hostApply(c, directory, commentedItems(directory, "idx_items_kind"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, kind) VALUES (1, 'tool'u)"), qt.IsNil)
	// A setting the declaration does not state, which a removal and an
	// addition would lose.
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+absolute+"` ALTER INDEX `idx_items_kind` SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)"), qt.IsNil)

	renamed := commentedItems(directory, "idx_items_by_kind")
	for _, run := range []struct{ name, want string }{
		// One alteration, then the comment cleared under the old name and set
		// under the new one.
		{"the rename", "(?s)Planned changes:\nNative operation: \\S+/row-table-alter-operation\n(  [^\n]*\n)*" +
			"COMMENT ON INDEX `idx_items_kind` ON `[^`]+` IS NULL;\nCOMMENT ON INDEX `idx_items_by_kind` ON `[^`]+` IS 'By kind';\n.*Schema apply completed successfully.*"},
		{"a run after the rename", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, renamed)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(output, qt.Not(qt.Matches), `(?s).*Drops a secondary index.*`, qt.Commentf("%s", run.name))
	}
	indexes, attributes := describedItems(c, opened, absolute)
	c.Assert(indexes, qt.HasLen, 1)
	index := indexes["idx_items_by_kind"]
	c.Assert(index.GetIndexColumns(), qt.DeepEquals, []string{"kind"})
	c.Assert(index.GetGlobalIndex().GetSettings().GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(3))
	c.Assert(attributes, qt.DeepEquals, map[string]string{"ptah.comment.index.idx_items_by_kind": "By kind"})
	var count uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute+"` VIEW `idx_items_by_kind` WHERE kind = 'tool'u").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(1))
}
