//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// itemsIndexes are the indexes the host's global-index tests declare on
// items: a synchronous one on kind, an asynchronous one on price that covers
// kind and sku, and a unique one on sku.
const itemsIndexes = ", INDEX idx_items_kind GLOBAL SYNC ON (kind), INDEX idx_items_price GLOBAL ASYNC ON (price) COVER (kind, sku), " +
	"INDEX uq_items_sku GLOBAL UNIQUE SYNC ON (sku)"

// newItemsFixture is a facet fixture whose records also hold the columns of
// the host's items table and the indexes itemsIndexes states. Its provider
// renders the TEXT form too.
func newItemsFixture(c *qt.C) *facetFixture {
	c.Helper()
	fixture := newFacetFixtureListing(c, ", sku Utf8, kind Utf8, price Int64"+itemsIndexes, "", provider.TextUnitsFeature)
	fixture.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true},
		{Name: "payload", Type: "Utf8", Nullable: true}, {Name: "sku", Type: "Utf8", Nullable: true}, {Name: "kind", Type: "Utf8", Nullable: true},
		{Name: "price", Type: "Int64", Nullable: true}}
	return fixture
}

// itemsIndexSet states the three indexes of items, each with the settings
// fields settings gives it by name; an index settings leaves out states none.
func itemsIndexSet(settings map[string]string) string {
	return declaredIndexes(
		indexWithSettingsOn("idx_items_kind", "globalIndex", `["kind"]`, "", settings["idx_items_kind"]),
		indexWithSettingsOn("idx_items_price", "globalAsyncIndex", `["price"]`, `["kind","sku"]`, settings["idx_items_price"]),
		indexWithSettingsOn("uq_items_sku", "globalUniqueIndex", `["sku"]`, "", settings["uq_items_sku"]))
}

// indexWithSettingsOn states index name of the given kind on keys, covering
// cover, with the given settings fields, or with none for an empty one.
func indexWithSettingsOn(name, kind, keys, cover, settings string) string {
	text := `"name":"` + name + `","indexColumns":` + keys + `,"` + kind + `":{`
	if settings != "" {
		text += `"settings":{` + settings + `}`
	}
	text += "}"
	if cover != "" {
		text += `,"dataColumns":` + cover
	}
	return text
}

// heldSettingsOf is the partitioning and read replicas the provider reads back
// for the named index, as JSON.
func (f *facetFixture) heldSettingsOf(c *qt.C, name string) string {
	c.Helper()
	held := f.heldIndexes(c)[name]
	c.Assert(held, qt.IsNotNil, qt.Commentf("index %s", name))
	encoded, err := protojson.Marshal(tablecreate.HeldIndexSettings(held))
	c.Assert(err, qt.IsNil)
	return string(encoded)
}

// TestGlobalIndexSettingsRoundTripSubprocess is the host's
// TestYDBGlobalIndexes_PartitioningRoundTrip for indexes the table holds. It
// gives a synchronous, an asynchronous covering and a unique index their
// settings, reads them back from the server and plans nothing after; then it
// changes the settings in place, one index back to YDB's defaults by
// declaring them. A setting a declaration leaves out keeps what the index
// holds, so dropping a maximum, or every declaration, plans nothing. Each step
// ends with nothing left to plan, and planning the same declaration again
// plans nothing too.
//
// The host also creates the indexes with their settings in one plan, a CREATE
// TABLE and an ALTER INDEX after it. A new table takes its global indexes from
// the common declaration here, which states no settings, so the indexes are
// created with the table first; TestGlobalIndexAddedWithItsSettingsSubprocess
// adds one with its settings in one plan.
func TestGlobalIndexSettingsRoundTripSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newItemsFixture(c)

	tuned := itemsIndexSet(map[string]string{
		"idx_items_kind":  `"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"}`,
		"idx_items_price": `"partitioningSettings":{"partitioningBySize":"DISABLED"},"readReplicasSettings":{"perAzReadReplicasCount":"1"}`,
		"uq_items_sku":    `"partitioningSettings":{"partitionSizeMb":"512","minPartitionsCount":"2"}`,
	})
	plan := fixture.indexPlan(c, tuned)
	names, _ := fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-index-settings/000000", "row-table-index-settings/000001", "row-table-index-settings/000002"})
	fixture.run(c, "settings-tuned", plan)
	c.Assert(fixture.compareIndexes(c, tuned).Changes, qt.HasLen, 0)
	c.Assert(fixture.heldSettingsOf(c, "idx_items_kind"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"}}`))
	c.Assert(fixture.heldSettingsOf(c, "idx_items_price"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"DISABLED","minPartitionsCount":"1"},"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`))
	c.Assert(fixture.heldSettingsOf(c, "uq_items_sku"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"512","partitioningByLoad":"DISABLED","minPartitionsCount":"2"}}`))

	// In place: one ALTER INDEX per changed index, and each names every
	// setting, because setting AUTO_PARTITIONING_BY_SIZE resets the size and
	// the minimum the index holds.
	retuned := itemsIndexSet(map[string]string{
		"idx_items_kind":  `"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"4","maxPartitionsCount":"9"}`,
		"idx_items_price": `"partitioningSettings":{"partitioningBySize":"ENABLED"},"readReplicasSettings":{"perAzReadReplicasCount":"0"}`,
		"uq_items_sku":    `"partitioningSettings":{"partitionSizeMb":"512","minPartitionsCount":"2"}`,
	})
	plan = fixture.indexPlan(c, retuned)
	_, statements := fixture.planned(c, plan)
	c.Assert(statements, qt.DeepEquals, []string{
		"ALTER TABLE `<dir>/records` ALTER INDEX `idx_items_kind` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
			"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = ENABLED, " +
			"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9);",
		"ALTER TABLE `<dir>/records` ALTER INDEX `idx_items_price` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
			"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, " +
			"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, READ_REPLICAS_SETTINGS = \"PER_AZ:0\");",
	})
	fixture.run(c, "settings-retuned", plan)
	c.Assert(fixture.compareIndexes(c, retuned).Changes, qt.HasLen, 0)
	held := tablecreate.HeldIndexSettings(fixture.heldIndexes(c)["idx_items_price"])
	c.Assert(tablecreate.SameReplicas(held.GetReadReplicasSettings(), nil), qt.IsTrue)
	// The server reads the replicas back as a count of zero, which is none.
	c.Assert(fixture.heldSettingsOf(c, "idx_items_price"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"},`+
			`"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`))

	// A maximum the declaration leaves out stays, as YDB has no way to remove
	// one, and so does every setting once no index declares any.
	c.Assert(fixture.compareIndexes(c, itemsIndexSet(map[string]string{
		"idx_items_kind": `"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"4"}`,
		"uq_items_sku":   `"partitioningSettings":{"partitionSizeMb":"512","minPartitionsCount":"2"}`,
	})).Changes, qt.HasLen, 0)
	c.Assert(fixture.compareIndexes(c, itemsIndexSet(nil)).Changes, qt.HasLen, 0)
	c.Assert(fixture.heldSettingsOf(c, "idx_items_kind"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"4","maxPartitionsCount":"9"}}`))
}

// TestGlobalIndexAddedWithItsSettingsSubprocess is the partitioning half of
// the host's TestYDBGlobalIndexes_AddedToATableThatExists. An index added to a
// table that holds rows, with its settings, in one plan, is an ADD INDEX
// without them and an ALTER INDEX after it that gives them over the settings
// YDB gives a new index. The index reads back with the declared settings and
// the next plan has nothing to change.
func TestGlobalIndexAddedWithItsSettingsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixtureListing(c, ", sku Utf8, kind Utf8, price Int64, INDEX idx_items_kind GLOBAL SYNC ON (kind), INDEX uq_items_sku GLOBAL UNIQUE SYNC ON (sku)",
		"", provider.TextUnitsFeature)
	fixture.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true},
		{Name: "payload", Type: "Utf8", Nullable: true}, {Name: "sku", Type: "Utf8", Nullable: true}, {Name: "kind", Type: "Utf8", Nullable: true},
		{Name: "price", Type: "Int64", Nullable: true}}
	c.Assert(fixture.exec(c, "UPSERT INTO "+fixture.recordsPath()+" (id, sku, kind, price) VALUES (1, 'a'u, 'tool'u, 10)"), qt.IsNil)

	declared := itemsIndexSet(map[string]string{"idx_items_price": `"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"2"}`})
	plan := fixture.indexPlan(c, declared)
	names, statements := fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-add-index/000000", "row-table-index-settings/000000"})
	c.Assert(statements, qt.DeepEquals, []string{
		"ALTER TABLE `<dir>/records` ADD INDEX `idx_items_price` GLOBAL ASYNC ON (`price`) COVER (`kind`, `sku`);",
		"ALTER TABLE `<dir>/records` ALTER INDEX `idx_items_price` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
			"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);",
	})
	fixture.run(c, "added-with-settings", plan)
	c.Assert(fixture.heldSettingsOf(c, "idx_items_price"), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2"}}`))
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
}
