//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// layoutRebuilds are the cases of the host's
// TestYDBTablePartitioning_ChangesOnlyARebuildMakes: the table attributes the
// table is created with, the starting layout declared after, the words that
// refuse it without a rebuild, and the settings the rebuilt table reads back
// with. YDB keeps no record of a layout, and the minimum it set from the
// layout reads back; every other setting the declaration leaves out keeps
// what the old table held, a key bloom filter nobody set included, which a
// table reports as no value.
var layoutRebuilds = []struct {
	name, before, after, refusal, partitioning, bloom string
}{
	{"a starting layout", "", ` uniform_partitions="4"`,
		"it declares UNIFORM_PARTITIONS = 4, which YDB takes only when it creates a table", "by_size=ENABLED size=2048 by_load=DISABLED min=4 max=0", ""},
	{"a starting layout over held settings", ` auto_partitioning_by_size="DISABLED" auto_partitioning_max_partitions_count="9" key_bloom_filter="ENABLED"`,
		` partition_at_keys="(10)"`, "it declares PARTITION_AT_KEYS with one split point, which YDB takes only when it creates a table",
		"by_size=DISABLED size=0 by_load=DISABLED min=2 max=9", "ENABLED"},
}

// Ported from the host's TestYDBTablePartitioning_ChangesOnlyARebuildMakes
// (the rebuild): asked for a rebuild, a starting layout on a table created
// without it recreates the table with the layout and the held value of every
// other setting, keeps its rows, and leaves nothing to plan.
func TestHostRebuildsATableForAStartingLayout_HappyPath(t *testing.T) {
	for _, test := range layoutRebuilds {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			emptyAtCleanup(c, fixture, path.Join(database, directory))
			applyUntilSynced(c, directory, "go", replaceDirectory(partitionedItems(test.before), directory))
			c.Assert(execFixtureSchema(c.Context(), fixture.DB, "UPSERT INTO `"+absolute+"` (id, code, label) VALUES (1ul, 'a'u, 'one'u), (2ul, 'b'u, 'two'u)"), qt.IsNil)

			applyUntilSynced(c, directory, "go", replaceDirectory(partitionedItems(test.after), directory), "--allow-table-rebuild")

			held := heldStorage(c, fixture, absolute)
			c.Assert(held.Partitioning, qt.Equals, test.partitioning)
			c.Assert(held.KeyBloomFilter, qt.Equals, test.bloom)
			c.Assert(rowCount(c, fixture, absolute, "label IS NOT NULL"), qt.Equals, int64(2))
			c.Assert(hostPlanned(c, directory, "go", replaceDirectory(partitionedItems(test.after), directory)), qt.HasLen, 0)
		})
	}
}

// Ported from the host's TestYDBTablePartitioning_ChangesOnlyARebuildMakes
// (the refusal): without a rebuild the layout is refused with the embedded
// engine's words, which name the table and the flag that asks for a rebuild,
// and the table keeps its settings and its rows.
func TestHostRefusesAStartingLayoutWithoutARebuild_FailurePath(t *testing.T) {
	for _, test := range layoutRebuilds {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			emptyAtCleanup(c, fixture, path.Join(database, directory))
			applyUntilSynced(c, directory, "go", replaceDirectory(partitionedItems(test.before), directory))
			c.Assert(execFixtureSchema(c.Context(), fixture.DB, "UPSERT INTO `"+absolute+"` (id, code, label) VALUES (1ul, 'a'u, 'one'u), (2ul, 'b'u, 'two'u)"), qt.IsNil)
			before := heldStorage(c, fixture, absolute)

			refused, err := hostApplySource(c, directory, "go", replaceDirectory(partitionedItems(test.after), directory))

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
			c.Assert(refused, qt.Contains, `table "`+directory+`/items": `+test.refusal)
			c.Assert(refused, qt.Contains, "; YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild")
			c.Assert(heldStorage(c, fixture, absolute), qt.DeepEquals, before)
			c.Assert(rowCount(c, fixture, absolute, ""), qt.Equals, int64(2))
		})
	}
}
