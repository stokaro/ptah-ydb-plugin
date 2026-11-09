//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// renamedIndexItems declares items in directory as the host's global-index
// tests do: a synchronous index on kind, an asynchronous index on price that
// covers kind and sku, named priceIndex, and a unique index on sku, named
// skuIndex.
func renamedIndexItems(directory, priceIndex, skuIndex string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"sku\" type=\"TEXT\"\n\t//ptah:schema:index name=\"" + skuIndex + "\" fields=\"sku\" unique=\"true\"\n\tSKU string\n" +
		"\t//ptah:schema:field name=\"kind\" type=\"TEXT\"\n\t//ptah:schema:index name=\"idx_items_kind\" fields=\"kind\"\n\tKind string\n" +
		"\t//ptah:schema:field name=\"price\" type=\"BIGINT\"\n" +
		"\t//ptah:schema:index name=\"" + priceIndex + "\" fields=\"price\" type=\"async\" include=\"kind,sku\"\n\tPrice int64\n}\n"
}

// Ported from the host's TestYDBGlobalIndexes_RenameRoundTrip, for what
// TestHostRenamesAnIndexInPlace_HappyPath does not hold: an asynchronous
// covering index and a unique index renamed in one plan, each in place, and
// both renamed back in the next. The asynchronous index keeps its kind, the
// columns it covers and the partitioning an operator gave it; the unique
// index keeps its kind, still refuses a second row with the same sku, and
// reads through its new name. Every run after a rename is synced.
//
// The host's test also changes the asynchronous index's declared
// partitioning beside the rename back. A Go source declares no index setting
// this provider reads, so that part is not ported (stokaro/ptah-ydb-plugin#186).
func TestHostRenamesIndexesOfEveryKindInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, absolute := itemsFixture(c)
	output, err := hostApply(c, directory, renamedIndexItems(directory, "idx_items_price", "uq_items_sku"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, sku, kind, price) VALUES (1, 'a'u, 'tool'u, 10), (2, 'b'u, 'part'u, 20)"), qt.IsNil)
	// A setting the declaration does not state, which a removal and an
	// addition would lose.
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+absolute+"` ALTER INDEX `idx_items_price` SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)"), qt.IsNil)

	const (
		// Two alterations of the table and nothing else: a removal and an
		// addition would be four, and the removals would print their reason.
		renamed = `(?s)(` + unitPrefix + `\S+/row-table-alter-operation [^\n]*\n){2}Planned schema changes:\n.*Schema apply completed successfully.*`
		synced  = `(?s).*Schema is synced, no changes to be made.*`
	)
	for _, run := range []struct{ name, priceIndex, skuIndex, want string }{
		{"the renames", "idx_items_by_price", "uq_items_code", renamed},
		{"a run after the renames", "idx_items_by_price", "uq_items_code", synced},
		{"the renames back", "idx_items_price", "uq_items_sku", renamed},
		{"a run after the renames back", "idx_items_price", "uq_items_sku", synced},
	} {
		output, err := hostApply(c, directory, renamedIndexItems(directory, run.priceIndex, run.skuIndex))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(output, qt.Not(qt.Matches), `(?s).*Drops a secondary index.*`, qt.Commentf("%s", run.name))
		c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{
			run.priceIndex: {{"price"}, {"kind", "sku"}}, run.skuIndex: {{"sku"}, nil}, "idx_items_kind": {{"kind"}, nil},
		}, qt.Commentf("%s", run.name))
		c.Assert(heldIndexKinds(c, opened, absolute), qt.DeepEquals, map[string]options.IndexType{
			run.priceIndex: options.IndexTypeGlobalAsync, run.skuIndex: options.IndexTypeGlobalUnique, "idx_items_kind": options.IndexTypeGlobal,
		}, qt.Commentf("%s", run.name))
		indexes, _ := describedItems(c, opened, absolute)
		c.Assert(indexes[run.priceIndex].GetGlobalAsyncIndex().GetSettings().GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(3),
			qt.Commentf("%s", run.name))
		var count uint64
		c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute+"` VIEW `"+run.skuIndex+"` WHERE sku = 'b'u").Scan(&count), qt.IsNil,
			qt.Commentf("%s", run.name))
		c.Assert(count, qt.Equals, uint64(1), qt.Commentf("%s", run.name))
	}
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "INSERT INTO `"+absolute+"` (id, sku) VALUES (3, 'b'u)"), qt.IsNotNil)
	assertRowCount(c, opened, absolute, 2)
}
