//go:build integration

package integration_test

import (
	"fmt"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rebuiltItems declares table items keyed on an unsigned id and a text code,
// with the table attributes given, a column n of the type given, a label and
// the index directive given on the label.
func rebuiltItems(attributes, nType, index string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\" primary_key=\"id,code\"" + attributes + "\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"code\" type=\"Utf8\" not_null=\"true\"\n\tCode string\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n" + index + "\tLabel string\n}\n"
}

// rebuildRuns applies after through the host with table rebuilds allowed,
// requires that the run rebuilt the table, and that the next run, with or
// without the rebuild allowed, finds nothing to change.
func rebuildRuns(c *qt.C, directory, format, after string) {
	c.Helper()
	output, err := hostApplySelected(c, directory, format, after, "--allow-table-rebuild")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*row-table-copy-operation.*row-table-rename-operation.*Schema apply completed successfully.*`)
	c.Assert(hostPlanned(c, directory, format, after, "--allow-table-rebuild"), qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, format, after), qt.HasLen, 0)
}

// Ported from the host's TestYDBTablePartitioning_RebuildCarriesTheSettings:
// a table rebuilt for a column type change is created with the settings its
// declaration states, and nothing is left to plan.
func TestHostRebuildCarriesTheDeclaredSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	settings := ` auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="3" read_replicas_settings="ANY_AZ:1"`
	created, err := hostApplySource(c, directory, "go", replaceDirectory(rebuiltItems(settings, "Int32", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, code, n) VALUES (1ul, 'a'u, 7)")
	c.Assert(err, qt.IsNil)

	rebuildRuns(c, directory, "go", replaceDirectory(rebuiltItems(settings, "Int64", ""), directory))

	held := heldStorage(c, opened, absolute)
	c.Assert(held.Partitioning, qt.Equals, "by_size=ENABLED size=2048 by_load=ENABLED min=3 max=0")
	c.Assert(held.ReadReplicas, qt.Equals, "ANY_AZ:1")
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
}

// Ported from the host's TestYDBTablePartitioning_RebuildKeepsWhatTheDeclarationLeavesOut,
// for the table: a table whose declaration names none of its settings is
// rebuilt for a column type change, and the new table takes every setting
// the old one holds, so the rebuild changes no setting nobody declared, and
// nothing is left to plan. The index half is
// TestHostRebuildKeepsTheIndexSettingsADeclarationLeavesOut_HappyPath.
func TestHostRebuildKeepsTheTableSettingsADeclarationLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	tuned := ` auto_partitioning_by_size="DISABLED" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="3"` +
		` auto_partitioning_max_partitions_count="9" read_replicas_settings="ANY_AZ:1" key_bloom_filter="ENABLED"`
	created, err := hostApplySource(c, directory, "go", replaceDirectory(rebuiltItems(tuned, "Int32", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, code, n) VALUES (1ul, 'a'u, 7)")
	c.Assert(err, qt.IsNil)
	want := storedTable{Families: []string{"default compression=off"}, Partitioning: "by_size=DISABLED size=0 by_load=ENABLED min=3 max=9",
		ReadReplicas: "ANY_AZ:1", KeyBloomFilter: "ENABLED"}
	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, want)

	rebuildRuns(c, directory, "go", replaceDirectory(rebuiltItems("", "Int64", ""), directory))

	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, want)
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
}

// ttlItems declares table items keyed on an unsigned id, with a column n of
// the type given and a TTL after interval on a Timestamp column ts.
func ttlItems(interval, nType string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\" platform.ydb.row_deletion_column=\"ts\" platform.ydb.row_deletion_interval=\"" +
		interval + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" not_null=\"true\" primary=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"ts\" type=\"Timestamp\"\n\tTS int64\n}\n"
}

// Ported from the host's TestYDBRebuild_CarriesTheTTL: a table with a TTL is
// rebuilt for a column type change, and the declaration changes the TTL in
// the same plan. The plan `schema apply` runs is the rebuild alone, whose new
// table is created with the declared TTL, so no step sets the TTL apart, and
// the rebuilt table holds the declared TTL and both rows, with nothing left to
// plan. The host's half that reads the migration file is not here: `migrations
// generate` has no down migration for the TTL change until this provider
// reverses a change of a table's own settings (stokaro/ptah-ydb-plugin#167).
func TestHostRebuildCarriesTheTTL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	created, err := hostApplySource(c, directory, "go", replaceDirectory(ttlItems("P1D", "Int32"), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n, ts) VALUES (1ul, 7, CurrentUtcTimestamp()), (2ul, NULL, NULL)"), qt.IsNil)
	after := replaceDirectory(ttlItems("P3D", "Int64"), directory)

	planned := hostPlanned(c, directory, "go", after, "--allow-table-rebuild")

	c.Assert(planned, qt.DeepEquals, []string{
		"row-table-create-operation " + directory + ".__ptah_rebuild_items", "row-table-copy-operation " + directory + ".__ptah_rebuild_items",
		"row-table-rename-operation " + directory + ".items", "row-table-rename-operation " + directory + ".__ptah_rebuild_items",
		"table-drop-operation " + directory + ".__ptah_replaced_items",
	})
	rebuildRuns(c, directory, "go", after)
	c.Assert(heldStorage(c, opened, absolute).TTL, qt.Equals, "ts 259200s")
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}

// indexStorage is the partitioning and the read replicas YDB keeps for an
// index, as partitioningText and replicasText write a table's.
func indexStorage(index *native.TableIndexDescription) string {
	settings := tablecreate.HeldIndexSettings(index)
	partitioning := settings.GetPartitioningSettings()
	return fmt.Sprintf("by_size=%s size=%d by_load=%s min=%d max=%d replicas=%s", partitioning.GetPartitioningBySize(), partitioning.GetPartitionSizeMb(),
		partitioning.GetPartitioningByLoad(), partitioning.GetMinPartitionsCount(), partitioning.GetMaxPartitionsCount(), settings.GetReadReplicasSettings())
}

// Ported from the host's TestYDBTablePartitioning_RebuildKeepsWhatTheDeclarationLeavesOut,
// for the index: the table and its index hold settings nobody declared, the
// index's set with ALTER INDEX since a source states no index settings here,
// and the table is rebuilt for a column type change. The new index takes the
// settings the old one holds, with an index settings step on the new table
// before the copy, so the rebuild changes no setting nobody declared, the
// rows and the reads through the index survive, and nothing is left to plan.
func TestHostRebuildKeepsTheIndexSettingsADeclarationLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	tuned := ` auto_partitioning_by_size="DISABLED" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="3"` +
		` auto_partitioning_max_partitions_count="9" read_replicas_settings="ANY_AZ:1" key_bloom_filter="ENABLED"`
	index := "\t//ptah:schema:index name=\"items_label\" fields=\"label\"\n"
	created, err := hostApplySource(c, directory, "go", replaceDirectory(rebuiltItems(tuned, "Int32", index), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+absolute+"` ALTER INDEX `items_label` SET (AUTO_PARTITIONING_BY_LOAD = ENABLED, "+
		"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2)"), qt.IsNil)
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, code, n, label) VALUES (1ul, 'a'u, 7, 'tool'u)")
	c.Assert(err, qt.IsNil)
	wantTable := storedTable{Families: []string{"default compression=off"}, Partitioning: "by_size=DISABLED size=0 by_load=ENABLED min=3 max=9",
		ReadReplicas: "ANY_AZ:1", KeyBloomFilter: "ENABLED"}
	const wantIndex = "by_size=ENABLED size=2048 by_load=ENABLED min=2 max=0 replicas=<nil>"
	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, wantTable)
	indexes, _ := describedItems(c, opened, absolute)
	c.Assert(indexStorage(indexes["items_label"]), qt.Equals, wantIndex)

	output, err := hostApplySelected(c, directory, "go", replaceDirectory(rebuiltItems("", "Int64", index), directory), "--allow-table-rebuild")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*row-table-create-operation.*row-table-index-settings-operation.*row-table-copy-operation.*row-table-rename-operation.*`+
		`Schema apply completed successfully.*`)
	c.Assert(hostPlanned(c, directory, "go", replaceDirectory(rebuiltItems("", "Int64", index), directory), "--allow-table-rebuild"), qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, "go", replaceDirectory(rebuiltItems("", "Int64", index), directory)), qt.HasLen, 0)

	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, wantTable)
	indexes, _ = describedItems(c, opened, absolute)
	c.Assert(indexes, qt.HasLen, 1)
	c.Assert(indexStorage(indexes["items_label"]), qt.Equals, wantIndex)
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
	var count uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute+"` VIEW `items_label` WHERE label = 'tool'u").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(1))
}
