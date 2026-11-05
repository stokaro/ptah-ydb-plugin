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
// layout, a column table's storage, changefeeds and native indexes, and a
// column's family, compression and encoding. A facet
// that holds such a setting beside its TTL is written in part, and the loss
// names what the document leaves out, in the words the embedded engine
// reports it with; one that holds it without a TTL is lost. A facet that
// holds neither, such as the empty changefeed set and native index set a read
// reports for a table without either and the facet of a column of the default
// family, is written by the table's part with no override, which the host
// writes as no block: a table that writes no block states nothing about its
// TTL, changefeeds, native indexes or column families. Reading the
// document back keeps every setting it leaves out, since a declaration that
// leaves a setting out keeps what the table holds.
//
// The table's written facets share one part. Only the facets the platform
// block claims have a place in HCL; any other facet is a loss. Where the host
// reads the provider's index kinds, a native index set of vector and
// full-text indexes is written as an index block of each, with the settings
// the host's embedded engine writes there; a local index has no HCL
// declaration, since HCL cannot state the column table it belongs to.
func EncodeTableHCL(table Table) (TableResult, error) {
	var result TableResult
	var entries []*protocol.SourceEntry
	var owned []*protocol.EncodedSourcePart
	for _, value := range table.Values {
		id := value.GetValue().GetId()
		if parts, err := hclNativeIndexes(table, value.GetValue().GetValue()); err != nil || parts != nil {
			if err != nil {
				return TableResult{}, err
			}
			result.Written, owned = append(result.Written, id), append(owned, parts...)
			continue
		}
		written, losses, err := hclTableValue(value.GetValue().GetValue())
		if err != nil {
			return TableResult{}, err
		}
		if setting := strings.Join(losses, "; "); setting != "" {
			result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: id, Message: setting})
		}
		if written == nil {
			continue
		}
		result.Written = append(result.Written, id)
		entries = append(entries, written...)
	}
	if len(result.Written) == 0 {
		return result, nil
	}
	claim := tablesource.HCLPlatformBlock()
	result.Parts = []*protocol.EncodedSourcePart{{Fragment: &protocol.SourceFragment{Id: "table." + table.Binding.GetId() + ".platform",
		Selector: claim.GetSelector(), HclBlock: claim, BindingId: table.Binding.GetId(),
		Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}}}}
	result.Parts = append(result.Parts, owned...)
	return result, nil
}

// hclNativeIndexes writes a native index set of vector and full-text indexes
// as their index blocks, where the host reads the provider's index kinds, and
// returns nil for any other facet and any other set.
func hclNativeIndexes(table Table, envelope *protocol.Envelope) ([]*protocol.EncodedSourcePart, error) {
	if !table.IndexKinds || envelope.GetKind() != schema.IndexProperties {
		return nil, nil
	}
	normalized, err := schema.NormalizeDesired(envelope)
	if err != nil {
		return nil, err
	}
	var intent native.IndexSetIntent
	if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
		return nil, err
	}
	var parts []*protocol.EncodedSourcePart
	for _, declaration := range intent.GetIndexes() {
		if !tablecreate.NativeOnlyIndex(declaration.GetSpecification()) {
			return nil, nil
		}
		parts = append(parts, nativeOwned(declaration.GetSpecification()).part(table.Binding, protocol.SourceDocument_HCL))
	}
	return parts, nil
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
		losses := []string{"YDB column storage, hash partitioning and tiered TTL are not represented in HCL"}
		names := make([]string, 0, len(intent.GetLocalIndexes()))
		for _, index := range intent.GetLocalIndexes() {
			names = append(names, index.GetName())
		}
		if _, lost, err := neutralOrLost(names, "local index", "local indexes"); err == nil {
			losses = append(losses, lost...)
		}
		return ttlEntries(nil, &intent), losses, nil
	case schema.ChangefeedProperties, schema.IndexProperties:
		return hclSetValue(envelope.GetKind(), normalized.GetPayload())
	case schema.ColumnProperties:
		return hclColumnValue(normalized.GetPayload())
	default:
		return nil, []string{fmt.Sprintf("%s of %s %s has no HCL spelling", strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/"), "-", " "),
			owner.GetKind(), Subject(owner))}, nil
	}
}

// hclSetValue is what the platform block writes of a table's changefeed set
// or native index set, kind, whose normalized payload is payload: an empty
// set by writing nothing, and any other as a loss that names its objects.
func hclSetValue(kind string, payload []byte) ([]*protocol.SourceEntry, []string, error) {
	if kind == schema.ChangefeedProperties {
		var intent native.ChangefeedSetIntent
		if err := protojson.Unmarshal(payload, &intent); err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(intent.GetChangefeeds()))
		for _, feed := range intent.GetChangefeeds() {
			names = append(names, feed.GetName())
		}
		return neutralOrLost(names, "changefeed", "changefeeds")
	}
	var intent native.IndexSetIntent
	if err := protojson.Unmarshal(payload, &intent); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(intent.GetIndexes()))
	for _, index := range intent.GetIndexes() {
		names = append(names, index.GetSpecification().GetName())
	}
	return neutralOrLost(names, "native index", "native indexes")
}

// hclColumnValue is what the platform block writes of a column's facet,
// whose normalized payload is payload. A column of the default family states
// nothing, and a literal default and a Serial's sequence are the common
// column's, which the host writes; a family, a compression or an encoding is a
// loss at the column.
func hclColumnValue(payload []byte) ([]*protocol.SourceEntry, []string, error) {
	var intent native.ColumnIntent
	if err := protojson.Unmarshal(payload, &intent); err != nil {
		return nil, nil, err
	}
	switch {
	case intent.GetCompression() != nil || len(intent.GetEncoding()) != 0:
		return nil, []string{"its compression or encoding is not represented in HCL"}, nil
	case intent.GetFamily() != "" && intent.GetFamily() != defaultFamily:
		return nil, []string{fmt.Sprintf("its column family %s is not represented in HCL", intent.GetFamily())}, nil
	}
	return make([]*protocol.SourceEntry, 0), nil, nil
}

// neutralOrLost is how HCL writes a set of named objects of one kind: an empty
// set by writing nothing, and any other as a loss that names its objects.
func neutralOrLost(names []string, one, many string) ([]*protocol.SourceEntry, []string, error) {
	switch len(names) {
	case 0:
		return make([]*protocol.SourceEntry, 0), nil, nil
	case 1:
		return nil, []string{fmt.Sprintf("%s %s is not represented in HCL", one, names[0])}, nil
	}
	return nil, []string{fmt.Sprintf("%s %s are not represented in HCL", many, strings.Join(names, ", "))}, nil
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
