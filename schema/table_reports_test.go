package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/types/known/emptypb"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// documentedPartitioning is how YDB describes the partitioning of a new table
// or global index: split by size at 2048 MB, not by load, one partition at
// least and no maximum.
func documentedPartitioning() *Ydb_Table.PartitioningSettings {
	return &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048,
		PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}
}

// rowTable is a row table read with the documented partitioning and a default
// family that states nothing, changed by change.
func rowTable(change func(*native.DescribeTableResult)) *native.DescribeTableResult {
	table := &native.DescribeTableResult{PrimaryKey: []string{"id"}, PartitioningSettings: documentedPartitioning(),
		ColumnFamilies: []*native.ColumnFamily{{Name: "default", Compression: Ydb_Table.ColumnFamily_COMPRESSION_NONE}}}
	change(table)
	return table
}

// capturedTable is the observed row-table properties of table.
func capturedTable(c *qt.C, table *native.DescribeTableResult) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureTable(tableOwner(), table)
	c.Assert(err, qt.IsNil)
	return value
}

// capturedStorage is the observed column storage of a column table of three
// shards with ttl.
func capturedStorage(c *qt.C, ttl *native.ColumnTTLPolicy) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureColumnStorage(tableOwner(), &native.ColumnStorageObservation{HashColumns: []string{"id"}, Partitions: 3, DataChannelCount: 64, Ttl: ttl})
	c.Assert(err, qt.IsNil)
	return value
}

// enabledTTL is a column table's TTL on ts with tiers.
func enabledTTL(tiers ...*native.ColumnTTLTier) *native.ColumnTTLPolicy {
	return &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: &native.ColumnTTLEnabled{Column: "ts", Tiers: tiers}}}
}

// capturedIndexes is the observed index set of a table.
func capturedIndexes(c *qt.C, set *native.IndexObservations) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureIndexes(tableOwner(), set)
	c.Assert(err, qt.IsNil)
	return value
}

// commonIndex is a global secondary index the host reads as a common index,
// described with settings.
func commonIndex(name string, settings *native.GlobalIndexSettings) *native.TableIndexDescription {
	return &native.TableIndexDescription{Name: name, IndexColumns: []string{"label"}, Status: native.TableIndexDescription_STATUS_READY,
		Type: &native.TableIndexDescription_GlobalIndex{GlobalIndex: &native.GlobalIndex{Settings: settings}}}
}

// vectorIndex is a vector index with its level and posting tables.
func vectorIndex(name string) *native.ObservedIndex {
	implementation := func(table string, columns ...string) *native.ImplementationTable {
		description := &native.DescribeTableResult{PrimaryKey: columns[:2], PartitioningSettings: documentedPartitioning()}
		for _, column := range columns {
			description.Columns = append(description.Columns, &native.ColumnMeta{Name: column, Type: primitive(Ydb.Type_UINT64)})
		}
		return &native.ImplementationTable{Name: table, Description: description}
	}
	vector := &native.KMeansTreeSettings{Settings: &native.VectorIndexSettings{Metric: new(native.VectorIndexSettings_DISTANCE_COSINE),
		VectorType: new(native.VectorIndexSettings_VECTOR_TYPE_FLOAT), VectorDimension: new(uint32(2))}, Clusters: new(uint32(2)), Levels: new(uint32(1))}
	return &native.ObservedIndex{
		Description: &native.TableIndexDescription{Name: name, IndexColumns: []string{"emb"}, Status: native.TableIndexDescription_STATUS_READY,
			Type: &native.TableIndexDescription_GlobalVectorKmeansTreeIndex{GlobalVectorKmeansTreeIndex: &native.GlobalVectorKMeansTreeIndex{VectorSettings: vector}}},
		Tables: []*native.ImplementationTable{implementation("indexImplLevelTable", "__ydb_parent", "__ydb_id", "__ydb_centroid"),
			implementation("indexImplPostingTable", "__ydb_parent", "id")},
	}
}

// The counts of the built-in engine's table setting families, for a read and
// for the declaration the host converts it to before it counts, which count
// alike. A row table or a global index counts once where a setting differs
// from what YDB's documentation gives a new one.
func TestCountValue_TableSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	setHeld := indexObservation("held")
	setHeld.Description.Type = &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{}}
	setDocumented := indexObservation("documented")
	setDocumented.Tables[0].Description.PartitioningSettings = documentedPartitioning()
	for _, row := range []struct {
		name     string
		observed *protocol.Envelope
		want     []string
	}{
		{"a row table with the documented settings", capturedTable(c, rowTable(func(*native.DescribeTableResult) {})),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a row table described with no settings", capturedTable(c, &native.DescribeTableResult{PrimaryKey: []string{"id"}}),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a TTL on a date column", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.TtlSettings = &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "ts", ExpireAfterSeconds: 86400}}}
		})), []string{"ydb_ttl_tables=1", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a TTL on an epoch column", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.TtlSettings = &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_ValueSinceUnixEpoch{ValueSinceUnixEpoch: &Ydb_Table.ValueSinceUnixEpochModeSettings{
				ColumnName: "expiry", ColumnUnit: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS, ExpireAfterSeconds: 3600}}}
		})), []string{"ydb_ttl_tables=1", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a compressed default family and a family that states nothing", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies = []*native.ColumnFamily{{Name: "default", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4}, {Name: "cold", Compression: Ydb_Table.ColumnFamily_COMPRESSION_NONE}}
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=2", "ydb_table_partitioning=0"}},
		{"a plain default family and a compressed family", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies = append(table.ColumnFamilies, &native.ColumnFamily{Name: "cold", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4})
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=1", "ydb_table_partitioning=0"}},
		{"a default family on a storage pool of its own", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies[0].Data = &Ydb_Table.StoragePool{Media: "ssd"}
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=1", "ydb_table_partitioning=0"}},
		{"a default family kept in memory", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies[0].KeepInMemory = Ydb.FeatureFlag_ENABLED
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=1", "ydb_table_partitioning=0"}},
		{"a default family in the in-memory cache", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies[0].CacheMode = native.ColumnFamily_CACHE_MODE_IN_MEMORY
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=1", "ydb_table_partitioning=0"}},
		{"a default family in the regular cache", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ColumnFamilies[0].CacheMode = native.ColumnFamily_CACHE_MODE_REGULAR
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"split by load", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.PartitioningSettings.PartitioningByLoad = Ydb.FeatureFlag_ENABLED
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"not split by size", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.PartitioningSettings.PartitioningBySize, table.PartitioningSettings.PartitionSizeMb = Ydb.FeatureFlag_DISABLED, 0
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"split at another size", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.PartitioningSettings.PartitionSizeMb = 100
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"a minimum of four partitions", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.PartitioningSettings.MinPartitionsCount = 4
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"a maximum of nine partitions", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.PartitioningSettings.MaxPartitionsCount = 9
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"a read replica in each zone", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ReadReplicasSettings = &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: 1}}
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"read replicas across the zones", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ReadReplicasSettings = &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount{AnyAzReadReplicasCount: 2}}
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"no read replicas in each zone", capturedTable(c, rowTable(func(table *native.DescribeTableResult) {
			table.ReadReplicasSettings = &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{}}
		})), []string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a key bloom filter", capturedTable(c, rowTable(func(table *native.DescribeTableResult) { table.KeyBloomFilter = Ydb.FeatureFlag_ENABLED })),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"a key bloom filter turned off", capturedTable(c, rowTable(func(table *native.DescribeTableResult) { table.KeyBloomFilter = Ydb.FeatureFlag_DISABLED })),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"a column table without a TTL", capturedStorage(c, nil),
			[]string{"ydb_ttl_tables=0", "ydb_column_tables=1", "ydb_tiered_ttl_tables=0"}},
		{"a column table whose TTL is off", capturedStorage(c, &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Disabled{Disabled: &emptypb.Empty{}}}),
			[]string{"ydb_ttl_tables=0", "ydb_column_tables=1", "ydb_tiered_ttl_tables=0"}},
		{"a column table whose TTL deletes rows", capturedStorage(c, enabledTTL(&native.ColumnTTLTier{ApplyAfterSeconds: 86400, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}})),
			[]string{"ydb_ttl_tables=1", "ydb_column_tables=1", "ydb_tiered_ttl_tables=0"}},
		{"a column table whose TTL moves rows to an external data source", capturedStorage(c, enabledTTL(
			&native.ColumnTTLTier{ApplyAfterSeconds: 86400, Action: &native.ColumnTTLTier_ExternalStorage{ExternalStorage: "/local/app/archive"}},
			&native.ColumnTTLTier{ApplyAfterSeconds: 604800, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}})),
			[]string{"ydb_ttl_tables=0", "ydb_column_tables=1", "ydb_tiered_ttl_tables=1"}},
		{"global indexes the set holds, read from their implementation tables", capturedIndexes(c, &native.IndexObservations{Indexes: []*native.ObservedIndex{setHeld, setDocumented}}),
			[]string{"ydb_index_partitioning=1", "ydb_vector_indexes=0"}},
		{"two vector indexes", capturedIndexes(c, &native.IndexObservations{Indexes: []*native.ObservedIndex{vectorIndex("v1"), vectorIndex("v2")}}),
			[]string{"ydb_index_partitioning=0", "ydb_vector_indexes=2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed, err := schema.CountValue(row.observed, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			c.Assert(countText(observed), qt.DeepEquals, row.want)
			counted, err := schema.CountValue(convertedDesired(c, row.observed), protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			c.Assert(countText(counted), qt.DeepEquals, row.want)
		})
	}
}

// A read counts the settings of the global indexes the host reads as common
// indexes from their descriptions. The declaration the host converts the read
// to holds none of them, so it counts none: through such a host the built-in
// engine's count of indexes with settings needs a model of the index's own.
func TestCountValue_CommonIndexes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	settings := func(partitioning *Ydb_Table.PartitioningSettings, replicas *Ydb_Table.ReadReplicasSettings) *native.GlobalIndexSettings {
		return &native.GlobalIndexSettings{PartitioningSettings: partitioning, ReadReplicasSettings: replicas}
	}
	minimum := documentedPartitioning()
	minimum.MinPartitionsCount, minimum.MaxPartitionsCount = 2, 5
	byLoad := documentedPartitioning()
	byLoad.PartitioningByLoad = Ydb.FeatureFlag_ENABLED
	read := capturedIndexes(c, &native.IndexObservations{CommonIndexes: []*native.TableIndexDescription{
		commonIndex("a", settings(minimum, nil)), commonIndex("b", settings(byLoad, nil)),
		commonIndex("c", settings(documentedPartitioning(), &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: 1}})),
		commonIndex("d", settings(documentedPartitioning(), nil)), commonIndex("e", nil),
	}})

	observed, err := schema.CountValue(read, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	desired, desiredErr := schema.CountValue(convertedDesired(c, read), protocol.ValueInput_DESIRED)

	c.Assert(countText(observed), qt.DeepEquals, []string{"ydb_index_partitioning=3", "ydb_vector_indexes=0"})
	c.Assert(desiredErr, qt.IsNil)
	c.Assert(countText(desired), qt.DeepEquals, []string{"ydb_index_partitioning=0", "ydb_vector_indexes=0"})
}

// desiredValue is a desired value of kind under model with payload.
func desiredValue(c *qt.C, kind string, model *protocol.ValueModel, payload string) *protocol.Envelope {
	c.Helper()
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: kind, SchemaVersion: model.SchemaVersion, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: tableOwner(), Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return value
}

// A declaration counts what it states the way a read is counted, and a
// starting layout, which only a declaration states, counts a table as stating
// its partitioning.
func TestCountValue_DeclaredTableSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	table, storage := schema.TableDesiredModel(), schema.NativeDesiredModel(schema.ColumnStorageProperties)
	for _, row := range []struct {
		name  string
		value *protocol.Envelope
		want  []string
	}{
		{"a table that states nothing", desiredValue(c, schema.TableProperties, table, `{"primaryKey":["id"]}`),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=0"}},
		{"uniform partitions", desiredValue(c, schema.TableProperties, table, `{"primaryKey":["id"],"uniformPartitions":"4"}`),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"split points", desiredValue(c, schema.TableProperties, table, `{"primaryKey":["id"],"partitionAtKeys":[{"values":["10"]}]}`),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=0", "ydb_table_partitioning=1"}},
		{"split by load and a family", desiredValue(c, schema.TableProperties, table,
			`{"primaryKey":["id"],"partitioningSettings":{"partitioningByLoad":"ENABLED"},"columnFamilies":[{"name":"cold","compression":"COMPRESSION_LZ4"}]}`),
			[]string{"ydb_ttl_tables=0", "ydb_column_families=1", "ydb_table_partitioning=1"}},
		{"a column table whose TTL deletes rows", desiredValue(c, schema.ColumnStorageProperties, storage,
			`{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`),
			[]string{"ydb_ttl_tables=1", "ydb_column_tables=1", "ydb_tiered_ttl_tables=0"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			counts, err := schema.CountValue(row.value, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			c.Assert(countText(counts), qt.DeepEquals, row.want)
		})
	}
}
