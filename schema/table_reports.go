package schema

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// The table setting families count what the host's built-in YDB engine
// counted. That engine read a table's or a global index's settings as the
// ones that differ from what YDB's documentation gives a new table or index,
// and counted the table or the index once where one did. A value here holds
// every setting the server describes, the documented ones included, in a read
// and in the declaration converted from it, so each count reads its value
// against those defaults: split by size at 2048 MB, not by load, at least one
// partition, no maximum, no read replicas and no key bloom filter.
const (
	documentedPartitionSizeMB = 2048
	documentedMinPartitions   = 1
	// defaultFamilyName is the family every row table has.
	defaultFamilyName = "default"
)

// rowTableSettings are the settings of a row table the counts read, from a
// declaration or an observation.
type rowTableSettings struct {
	ttl          *Ydb_Table.TtlSettings
	families     []*native.ColumnFamily
	partitioning *Ydb_Table.PartitioningSettings
	replicas     *Ydb_Table.ReadReplicasSettings
	bloomFilter  Ydb.FeatureFlag_Status
	storage      *Ydb_Table.StorageSettings
	attributes   int
	tiering      string
	temporary    bool
	// layout is a starting layout, which only a declaration states.
	layout bool
}

func rowTable(payload []byte, representation protocol.ValueInput_Representation) (rowTableSettings, error) {
	if representation == protocol.ValueInput_DESIRED {
		var table native.RowTableIntent
		err := unmarshalCounted(payload, &table)
		return rowTableSettings{ttl: table.GetTtlSettings(), families: table.GetColumnFamilies(), partitioning: table.GetPartitioningSettings(),
			replicas: table.GetReadReplicasSettings(), bloomFilter: table.GetKeyBloomFilter(),
			storage: table.GetStorageSettings(), attributes: len(table.GetAttributes()), tiering: table.GetTiering(), temporary: table.GetTemporary(),
			layout: table.GetUniformPartitions() != 0 || len(table.GetPartitionAtKeys()) != 0}, err
	}
	var table native.DescribeTableResult
	err := unmarshalCounted(payload, &table)
	return rowTableSettings{ttl: table.GetTtlSettings(), families: table.GetColumnFamilies(), partitioning: table.GetPartitioningSettings(),
		replicas: table.GetReadReplicasSettings(), bloomFilter: table.GetKeyBloomFilter(),
		storage: table.GetStorageSettings(), attributes: len(table.GetAttributes()), tiering: table.GetTiering(), temporary: table.GetTemporary()}, err
}

// ttlTable counts a row table with a TTL.
func ttlTable(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	table, err := rowTable(payload, representation)
	if table.ttl.GetMode() != nil {
		return 1, err
	}
	return 0, err
}

// statedFamilies counts a row table's column families, without a default
// family that holds what YDB gives a family stating no setting.
func statedFamilies(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	table, err := rowTable(payload, representation)
	return countStatedFamilies(table.families), err
}

func countStatedFamilies(families []*native.ColumnFamily) uint64 {
	var total uint64
	for _, family := range families {
		if family.GetName() != defaultFamilyName || !plainFamily(family) {
			total++
		}
	}
	return total
}

// plainFamily reports a family with no storage pool of its own, no
// compression, the regular cache and keep_in_memory off.
func plainFamily(family *native.ColumnFamily) bool {
	compression := family.GetCompression()
	cacheMode := family.GetCacheMode()
	return family.GetData().GetMedia() == "" &&
		(compression == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED || compression == Ydb_Table.ColumnFamily_COMPRESSION_NONE) &&
		(cacheMode == native.ColumnFamily_CACHE_MODE_UNSPECIFIED || cacheMode == native.ColumnFamily_CACHE_MODE_REGULAR) &&
		family.GetKeepInMemory() != Ydb.FeatureFlag_ENABLED
}

// tablePartitioning counts a row table whose partitioning, read replicas or
// key bloom filter differ from the documented ones, or whose declaration
// states a starting layout.
func tablePartitioning(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	table, err := rowTable(payload, representation)
	if tablePartitioningStated(table) {
		return 1, err
	}
	return 0, err
}

func tablePartitioningStated(table rowTableSettings) bool {
	return table.layout || table.bloomFilter == Ydb.FeatureFlag_ENABLED || partitioningStated(table.partitioning, table.replicas)
}

// rowTableOmission counts a row table only when DBML or inspection JSON loses
// a native setting. Primary keys and access are carried by common schema
// models; the server's documented defaults and a plain default family are
// not omitted settings.
func rowTableOmission(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	table, err := rowTable(payload, representation)
	if err != nil {
		return 0, err
	}
	storage := declaredStorage(table.storage)
	if table.ttl.GetMode() != nil || countStatedFamilies(table.families) != 0 || tablePartitioningStated(table) ||
		(storage != nil && !proto.Equal(storage, &Ydb_Table.StorageSettings{})) || table.attributes != 0 || table.tiering != "" || table.temporary {
		return 1, nil
	}
	return 0, nil
}

// partitioningStated reports settings of a table or a global index that
// differ from the documented ones. An unspecified switch, size or minimum is
// the documented one, and a partition size is not read where the table does
// not split by size.
func partitioningStated(partitioning *Ydb_Table.PartitioningSettings, replicas *Ydb_Table.ReadReplicasSettings) bool {
	bySize := partitioning.GetPartitioningBySize() != Ydb.FeatureFlag_DISABLED
	size := partitioning.GetPartitionSizeMb()
	minimum := partitioning.GetMinPartitionsCount()
	return !bySize || (size != 0 && size != documentedPartitionSizeMB) ||
		partitioning.GetPartitioningByLoad() == Ydb.FeatureFlag_ENABLED ||
		(minimum != 0 && minimum != documentedMinPartitions) || partitioning.GetMaxPartitionsCount() != 0 ||
		replicas.GetPerAzReadReplicasCount() != 0 || replicas.GetAnyAzReadReplicasCount() != 0
}

// columnTTLTiers returns the tiers of a column table's TTL, from a
// declaration or an observation.
func columnTTLTiers(payload []byte, representation protocol.ValueInput_Representation) ([]*native.ColumnTTLTier, error) {
	if representation == protocol.ValueInput_DESIRED {
		var storage native.ColumnStorageIntent
		err := unmarshalCounted(payload, &storage)
		return storage.GetTtl().GetEnabled().GetTiers(), err
	}
	var storage native.ColumnStorageObservation
	err := unmarshalCounted(payload, &storage)
	return storage.GetTtl().GetEnabled().GetTiers(), err
}

// deleteOnly reports a TTL of one tier that deletes rows, which YDB's table
// service describes as a row table's TTL.
func deleteOnly(tiers []*native.ColumnTTLTier) bool {
	return len(tiers) == 1 && tiers[0].GetDelete() != nil
}

// columnTTLTable counts a column table whose TTL only deletes rows: the
// built-in engine counted it among the tables with a TTL.
func columnTTLTable(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	tiers, err := columnTTLTiers(payload, representation)
	if deleteOnly(tiers) {
		return 1, err
	}
	return 0, err
}

// tieredTTLTable counts a column table whose TTL has any other tiers.
func tieredTTLTable(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	tiers, err := columnTTLTiers(payload, representation)
	if len(tiers) != 0 && !deleteOnly(tiers) {
		return 1, err
	}
	return 0, err
}

// indexSettings is what the counts read of one index of a table's index set:
// whether it is a vector index, and the partitioning and the read replicas of
// a global secondary index.
type indexSettings struct {
	vector, global bool
	partitioning   *Ydb_Table.PartitioningSettings
	replicas       *Ydb_Table.ReadReplicasSettings
}

// implementationTable is the table YDB keeps a global secondary index in.
const implementationTable = "indexImplTable"

// tableIndexes returns the indexes a table's index set holds. A global
// secondary index the host reads as a common index is not among them: an
// observation keeps its description in common_indexes, which the declaration
// converted from it drops, and its settings are counted from the index's own
// settings (IndexSettingsProperties), so a read and its declaration count
// alike. A global index the set holds is read from its implementation table,
// as the built-in engine read it.
func tableIndexes(payload []byte, representation protocol.ValueInput_Representation) ([]indexSettings, error) {
	var result []indexSettings
	if representation == protocol.ValueInput_DESIRED {
		var set native.IndexSetIntent
		err := unmarshalCounted(payload, &set)
		for _, index := range set.GetIndexes() {
			settings := declaredIndexSettings(index.GetSpecification())
			for _, table := range index.GetImplementation() {
				if table.GetName() == implementationTable {
					settings.partitioning, settings.replicas = table.GetSettings().GetPartitioningSettings(), table.GetSettings().GetReadReplicasSettings()
				}
			}
			result = append(result, settings)
		}
		return result, err
	}
	var set native.IndexObservations
	err := unmarshalCounted(payload, &set)
	for _, index := range set.GetIndexes() {
		settings := observedIndexSettings(index.GetDescription())
		for _, table := range index.GetTables() {
			if table.GetName() == implementationTable {
				settings.partitioning, settings.replicas = table.GetDescription().GetPartitioningSettings(), table.GetDescription().GetReadReplicasSettings()
			}
		}
		result = append(result, settings)
	}
	return result, err
}

func declaredIndexSettings(index *native.IndexIntent) indexSettings {
	switch kind := index.GetType().(type) {
	case *native.IndexIntent_GlobalIndex:
		return globalIndexSettings(kind.GlobalIndex.GetSettings())
	case *native.IndexIntent_GlobalAsyncIndex:
		return globalIndexSettings(kind.GlobalAsyncIndex.GetSettings())
	case *native.IndexIntent_GlobalUniqueIndex:
		return globalIndexSettings(kind.GlobalUniqueIndex.GetSettings())
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		return indexSettings{vector: true}
	default:
		return indexSettings{}
	}
}

func observedIndexSettings(index *native.TableIndexDescription) indexSettings {
	switch kind := index.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex:
		return globalIndexSettings(kind.GlobalIndex.GetSettings())
	case *native.TableIndexDescription_GlobalAsyncIndex:
		return globalIndexSettings(kind.GlobalAsyncIndex.GetSettings())
	case *native.TableIndexDescription_GlobalUniqueIndex:
		return globalIndexSettings(kind.GlobalUniqueIndex.GetSettings())
	case *native.TableIndexDescription_GlobalVectorKmeansTreeIndex:
		return indexSettings{vector: true}
	default:
		return indexSettings{}
	}
}

func globalIndexSettings(settings *native.GlobalIndexSettings) indexSettings {
	return indexSettings{global: true, partitioning: settings.GetPartitioningSettings(), replicas: settings.GetReadReplicasSettings()}
}

// indexPartitioning counts the global secondary indexes, synchronous,
// asynchronous and unique, whose partitioning or read replicas differ from
// the documented ones. A vector or full-text index is not one.
func indexPartitioning(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	indexes, err := tableIndexes(payload, representation)
	var total uint64
	for _, index := range indexes {
		if index.global && partitioningStated(index.partitioning, index.replicas) {
			total++
		}
	}
	return total, err
}

// indexSettingsStated counts a global index whose own settings differ from
// the documented ones: the settings a read holds, or the ones a declaration
// gives the index once YDB builds it, so a declaration converted from a read
// counts what the read counts.
func indexSettingsStated(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	held, err := heldIndexSettings(payload, representation)
	if partitioningStated(held.GetPartitioningSettings(), held.GetReadReplicasSettings()) {
		return 1, err
	}
	return 0, err
}

// heldIndexSettings returns the settings of an index from a read or from a
// declaration.
func heldIndexSettings(payload []byte, representation protocol.ValueInput_Representation) (*native.IndexSettingsObservation, error) {
	if representation == protocol.ValueInput_DESIRED {
		var intent native.IndexSettingsIntent
		err := unmarshalCounted(payload, &intent)
		return indexSettingsObservation(&intent), err
	}
	var held native.IndexSettingsObservation
	err := unmarshalCounted(payload, &held)
	return &held, err
}

// vectorIndexes counts the vector indexes of a table's index set.
func vectorIndexes(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	indexes, err := tableIndexes(payload, representation)
	var total uint64
	for _, index := range indexes {
		if index.vector {
			total++
		}
	}
	return total, err
}

// heldIndexes counts the indexes a table's index set holds: the vector and
// full-text indexes, and a global index the host does not read as a common
// index. The common indexes are not among them; see [tableIndexes].
func heldIndexes(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	indexes, err := tableIndexes(payload, representation)
	return uint64(len(indexes)), err
}

// localIndexes counts the local indexes of a column table.
func localIndexes(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	if representation == protocol.ValueInput_DESIRED {
		var storage native.ColumnStorageIntent
		err := unmarshalCounted(payload, &storage)
		return uint64(len(storage.GetLocalIndexes())), err
	}
	var storage native.ColumnStorageObservation
	err := unmarshalCounted(payload, &storage)
	return uint64(len(storage.GetLocalIndexes())), err
}
