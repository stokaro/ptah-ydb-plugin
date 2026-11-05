package sourceencode

import (
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// EncodeTableHCL writes the facets of one table as an HCL document holds
// them, the way the host's embedded engine writes them: the TTL of a row
// table, and of a column table whose TTL only deletes, as the overrides of
// the table's platform "ydb" block, which tablesource.HCLPlatformBlock
// claims. HCL has no spelling for anything else a facet holds: a row table's
// column families, partitioning, read replicas, key Bloom filter and starting
// layout, and a column table's storage. A facet that holds such a setting
// beside its TTL is written in part, and the loss names what the document
// leaves out, in the words the embedded engine reports it with; one that holds
// it without a TTL is lost. A facet that holds neither is written by a part
// with no override, which the host writes as no block: a table that writes no
// block states nothing about its TTL. Reading the document back keeps every
// setting it leaves out, since a declaration that leaves a setting out keeps
// what the table holds.
//
// Only the row-table and column-storage facets have a place in HCL; any other
// facet is a loss.
func EncodeTableHCL(table Table) (TableResult, error) {
	var result TableResult
	claim := tablesource.HCLPlatformBlock()
	for _, value := range table.Values {
		id := value.GetValue().GetId()
		entries, losses, err := hclTableValue(value.GetValue().GetValue())
		if err != nil {
			return TableResult{}, err
		}
		if setting := strings.Join(losses, "; "); setting != "" {
			result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: id, Message: setting})
		}
		if entries == nil {
			continue
		}
		result.Written = append(result.Written, id)
		result.Parts = append(result.Parts, &protocol.EncodedSourcePart{Fragment: &protocol.SourceFragment{Id: "table." + table.Binding.GetId() + ".platform",
			Selector: claim.GetSelector(), HclBlock: claim, BindingId: table.Binding.GetId(),
			Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}}})
	}
	return result, nil
}

// hclTableValue is what the platform block writes of one facet, and what the
// document loses of it. A nil entry list means nothing writes the facet; an
// empty one, a part with no override.
func hclTableValue(envelope *protocol.Envelope) ([]*protocol.SourceEntry, []string, error) {
	owner := envelope.GetOwner()
	normalized, err := schema.NormalizeDesired(envelope)
	if err != nil {
		return nil, nil, err
	}
	switch envelope.GetKind() {
	case schema.TableProperties:
		var intent native.RowTableIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return nil, nil, err
		}
		if setting := rowLoss(&intent); setting != "" {
			return nil, []string{fmt.Sprintf("table %s: %s", Subject(owner), setting)}, nil
		}
		losses, err := rowSettingLosses(&intent)
		if err != nil {
			return nil, nil, err
		}
		entries := ttlEntries(&intent, nil)
		if entries == nil && len(losses) == 0 {
			entries = make([]*protocol.SourceEntry, 0)
		}
		return entries, losses, nil
	case schema.ColumnStorageProperties:
		var intent native.ColumnStorageIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return nil, nil, err
		}
		return ttlEntries(nil, &intent), []string{"YDB column storage, hash partitioning and tiered TTL are not represented in HCL"}, nil
	default:
		return nil, []string{fmt.Sprintf("%s of %s %s has no HCL spelling", strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/"), "-", " "),
			owner.GetKind(), Subject(owner))}, nil
	}
}

// rowSettingLosses are what HCL leaves out of a row table's settings beside
// its TTL, as the embedded engine reports them: its stated column families,
// and its partitioning, read replicas, key Bloom filter and starting layout as
// YQL's WITH clause writes them. The primary key a facet repeats is the
// common table's, which the host writes.
func rowSettingLosses(intent *native.RowTableIntent) ([]string, error) {
	var losses []string
	var families []string
	for _, family := range intent.GetColumnFamilies() {
		if !plainDefaultFamily(family) {
			families = append(families, family.GetName())
		}
	}
	switch len(families) {
	case 0:
	case 1:
		losses = append(losses, fmt.Sprintf("column family %s is not represented in HCL", families[0]))
	default:
		losses = append(losses, fmt.Sprintf("column families %s are not represented in HCL", strings.Join(families, ", ")))
	}
	settings, err := tablecreate.SettingsText(stated(intent))
	if err != nil {
		return nil, err
	}
	if points := intent.GetPartitionAtKeys(); len(points) != 0 {
		settings = append(settings, "PARTITION_AT_KEYS = ("+splitPoints(points)+")")
	}
	if len(settings) != 0 {
		losses = append(losses, fmt.Sprintf("YDB table settings (%s) are not represented in HCL", strings.Join(settings, ", ")))
	}
	return losses, nil
}

// plainDefaultFamily reports the default family with no setting of its own,
// which every row table has and no source states, as the embedded engine
// reads it: no storage pool, no compression, a regular cache, and not kept in
// memory.
func plainDefaultFamily(family *native.ColumnFamily) bool {
	return family.GetName() == defaultFamily && family.GetData().GetMedia() == "" &&
		(family.GetCompression() == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED || family.GetCompression() == Ydb_Table.ColumnFamily_COMPRESSION_NONE) &&
		family.GetKeepInMemory() != Ydb.FeatureFlag_ENABLED &&
		(family.GetCacheMode() == native.ColumnFamily_CACHE_MODE_UNSPECIFIED || family.GetCacheMode() == native.ColumnFamily_CACHE_MODE_REGULAR)
}

// stated is a row table's settings without the ones YDB's documentation gives
// a new table, the baseline the embedded engine reports a table against:
// splitting by size at 2048 MB, not by load, at least one partition, and no
// key Bloom filter. A read reports them for every table.
func stated(intent *native.RowTableIntent) *native.RowTableIntent {
	settings := &native.RowTableIntent{PartitioningSettings: proto.CloneOf(intent.GetPartitioningSettings()), ReadReplicasSettings: intent.GetReadReplicasSettings(),
		KeyBloomFilter: intent.GetKeyBloomFilter(), UniformPartitions: intent.GetUniformPartitions()}
	if partitioning := settings.GetPartitioningSettings(); partitioning != nil {
		if partitioning.GetPartitioningBySize() == Ydb.FeatureFlag_ENABLED {
			partitioning.PartitioningBySize = Ydb.FeatureFlag_STATUS_UNSPECIFIED
		}
		if partitioning.GetPartitionSizeMb() == defaultPartitionSizeMB {
			partitioning.PartitionSizeMb = 0
		}
		if partitioning.GetPartitioningByLoad() == Ydb.FeatureFlag_DISABLED {
			partitioning.PartitioningByLoad = Ydb.FeatureFlag_STATUS_UNSPECIFIED
		}
		if partitioning.GetMinPartitionsCount() == 1 {
			partitioning.MinPartitionsCount = 0
		}
	}
	if settings.GetKeyBloomFilter() == Ydb.FeatureFlag_DISABLED {
		settings.KeyBloomFilter = Ydb.FeatureFlag_STATUS_UNSPECIFIED
	}
	return settings
}

// defaultPartitionSizeMB is the partition size YDB's documentation gives a new
// table that splits by size.
const defaultPartitionSizeMB = 2048
