package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"ptah.run/provider-sdk/modeljson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// TableDesiredModel describes row-table configuration and access constraints.
// Common columns and native index/changefeed models retain their own ownership.
func TableDesiredModel() *protocol.ValueModel { return proto.CloneOf(tableDesiredModel) }

var tableDesiredModel = buildTableDesiredModel()

func buildTableDesiredModel() *protocol.ValueModel {
	graph := make(map[string]any)
	describeMessage((&native.RowTableIntent{}).ProtoReflect().Descriptor(), graph)
	definition := map[string]any{
		"kind": TableProperties, "representation": "desired", "schema_version": 1,
		"encoding": "ydb-protojson", "message": "ptah.ydb.native.RowTableIntent", "messages": graph,
		"unknown_fields_and_enum_values": "refused", "null_settings": "refused recursively, including map entries",
		"canonicalization": "protojson followed by modeljson canonical JSON; key, family and grant order retained",
		"settings":         "ordered primary key, TTL, storage, column families, attributes, partitioning policy, bloom filter, read replicas, tiering, temporary lifetime and the starting layout of a new table",
		"starting_layout": "uniform_partitions is a count of at least 1; partition_at_keys lists split points of source text values; at most one of them; " +
			"YDB takes a layout only when it creates a table and keeps no record of it, so it is never observed",
		"access":        "present access declares the complete direct-grant set and requires the complete effective-permission set; owner omission leaves ownership unmanaged; constraints never grant permissions or infer inheritance settings",
		"omission":      "missing settings request native defaults and remain absent during normalization; missing access leaves access unmanaged",
		"primary_key":   "a declaration that states settings only, such as a YAML or Go source, omits the key and the effective key comes from the common columns; a stated key is distinct, ordered, and must agree with them",
		"runtime_state": "physical shard boundaries refuse conversion; statistics and transient identity are not allowed in either property model",
		"row_storage":   "this model requests row storage; observed unspecified storage is interpreted as row storage",
		"conversion":    "offline representation conversion establishes no observation coverage; permission planning is not advertised",
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(encoded)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: TableProperties, DisplayName: modelDisplayName(TableProperties), Representation: protocol.ValueInput_DESIRED, SchemaVersion: 1,
		Encoding: "ydb-protojson", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: TableProperties + "/desired-json-v1",
		SourceFormats: []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}}
}

// CaptureTableIntent writes the settings a source declares for a row table as
// the provider's desired facet of that table, normalized as a YAML or Go
// source's facet is.
func CaptureTableIntent(owner *protocol.Reference, intent *native.RowTableIntent) (*protocol.Envelope, error) {
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: TableProperties, Owner: proto.CloneOf(owner)}, TableDesiredModel(), intent, NormalizeDesired)
}

func normalizeTableIntent(value *protocol.Envelope) (*protocol.Envelope, error) {
	definition := TableDesiredModel()
	if value.GetProviderId() != ProviderID || value.GetKind() != definition.Kind || value.GetSchemaVersion() != definition.SchemaVersion ||
		value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("desired YDB table does not match its negotiated definition")
	}
	if err := validateModelOwner(value); err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.Payload)
	if err != nil {
		return nil, err
	}
	var intent native.RowTableIntent
	if err := rejectNullSettings(canonical, intent.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(canonical, &intent); err != nil {
		return nil, fmt.Errorf("invalid desired YDB row-table JSON")
	}
	if err := Known(&intent); err != nil {
		return nil, err
	}
	if err := validateTableIntent(&intent); err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(&intent)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return result, nil
}

func validateTableIntent(intent *native.RowTableIntent) error {
	if len(intent.PrimaryKey) != 0 && !distinctColumns(intent.PrimaryKey) {
		return fmt.Errorf("desired YDB table requires distinct ordered primary key columns")
	}
	families := make(map[string]bool)
	for _, family := range intent.ColumnFamilies {
		if family.GetName() == "" || families[family.Name] {
			return fmt.Errorf("invalid or duplicate YDB column family")
		}
		families[family.Name] = true
	}
	if partitioning := intent.PartitioningSettings; partitioning != nil {
		if len(partitioning.PartitionBy) != 0 && !distinctColumns(partitioning.PartitionBy) {
			return fmt.Errorf("invalid YDB partition columns")
		}
		if partitioning.MaxPartitionsCount != 0 && partitioning.MinPartitionsCount > partitioning.MaxPartitionsCount {
			return fmt.Errorf("YDB minimum partitions exceed maximum")
		}
	}
	if intent.UniformPartitions != 0 && len(intent.PartitionAtKeys) != 0 {
		return fmt.Errorf("uniform_partitions and partition_at_keys are both declared, which YDB refuses " +
			"(`Uniform partitions and partitions at keys settings are mutually exclusive`)")
	}
	if err := validateSplitPoints(intent.PartitionAtKeys); err != nil {
		return err
	}
	if err := validateTableTTL(intent.TtlSettings); err != nil {
		return err
	}
	return validateAccessIntent(intent.Access)
}

// validateSplitPoints checks the split points of partition_at_keys: each
// holds a value, and every value is valid UTF-8 without NUL.
func validateSplitPoints(points []*native.SplitPoint) error {
	for _, point := range points {
		if len(point.GetValues()) == 0 {
			return fmt.Errorf("a split point of partition_at_keys holds no value")
		}
		for _, value := range point.Values {
			if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
				return fmt.Errorf("a partition_at_keys value is not valid UTF-8 without NUL")
			}
		}
	}
	return nil
}

// validateTableTTL checks a declared row-table TTL, nil for none: an explicit
// mode with its column, and the unit of an epoch column.
func validateTableTTL(ttl *Ydb_Table.TtlSettings) error {
	if ttl == nil {
		return nil
	}
	switch mode := ttl.Mode.(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		if mode.DateTypeColumn.GetColumnName() == "" {
			return fmt.Errorf("YDB TTL requires a column")
		}
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		if mode.ValueSinceUnixEpoch.GetColumnName() == "" || mode.ValueSinceUnixEpoch.GetColumnUnit() == 0 {
			return fmt.Errorf("YDB epoch TTL requires a column and unit")
		}
	default:
		return fmt.Errorf("YDB TTL requires an explicit mode")
	}
	return nil
}

func validatePermissionSet(grants []*Ydb_Scheme.Permissions) error {
	subjects := make(map[string]bool)
	for _, grant := range grants {
		if grant.GetSubject() == "" || subjects[grant.Subject] || !distinctColumns(grant.PermissionNames) {
			return fmt.Errorf("invalid or duplicate YDB permission subject or names")
		}
		subjects[grant.Subject] = true
	}
	return nil
}

// ConvertTable changes row-table representations without treating physical
// topology as creation intent or inherited permissions as direct grants. A
// read becomes a declaration without the settings YDB gives every new table
// (see [WithoutNewTableDefaults]), as the host's embedded engine wrote a
// read's report against them, so an export or a rendering of the read names
// only what sets the table apart.
func ConvertTable(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	return convertTable(value, from, to, WithoutNewTableDefaults)
}

// restoredTable is [ConvertTable] keeping every setting a read reports: the
// declaration a reverse restores, as the host's embedded engine restored every
// setting a table held. A setting a declaration leaves out keeps what the
// table holds, so a reverse without the new-table defaults keeps each setting
// the forward change moved away from one. YDB can report a disabled key bloom
// filter as unspecified; the reverse must state DISABLED to turn off a filter
// the forward change enabled.
func restoredTable(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	return convertTable(value, from, to, func(intent *native.RowTableIntent) *native.RowTableIntent {
		result := proto.CloneOf(intent)
		if result.KeyBloomFilter == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
			result.KeyBloomFilter = Ydb.FeatureFlag_DISABLED
		}
		return result
	})
}

// convertTable is [ConvertTable] with declared choosing the settings a read's
// declaration states.
func convertTable(value *protocol.Envelope, from, to protocol.ValueInput_Representation, declared func(*native.RowTableIntent) *native.RowTableIntent) (*protocol.Envelope, error) {
	if value.GetKind() != TableProperties {
		return nil, fmt.Errorf("YDB table conversion requires a row-table model")
	}
	if from == protocol.ValueInput_OBSERVED && to == protocol.ValueInput_DESIRED {
		normalized, err := Normalize(value)
		if err != nil {
			return nil, err
		}
		var observed native.DescribeTableResult
		if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
			return nil, err
		}
		if len(observed.ShardKeyBounds) != 0 {
			return nil, fmt.Errorf("YDB physical shard boundaries cannot become schema split points")
		}
		return convertedEnvelope(value, TableDesiredModel(), declared(tableIntent(&observed)), NormalizeDesired)
	}
	if from == protocol.ValueInput_DESIRED && to == protocol.ValueInput_OBSERVED {
		normalized, err := normalizeTableIntent(value)
		if err != nil {
			return nil, err
		}
		var intent native.RowTableIntent
		if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
			return nil, err
		}
		return convertedEnvelope(value, model(TableProperties), tableObservation(&intent), Normalize)
	}
	return nil, fmt.Errorf("YDB table conversion requires distinct schema representations")
}

func tableIntent(observed *native.DescribeTableResult) *native.RowTableIntent {
	result := &native.RowTableIntent{PrimaryKey: slices.Clone(observed.PrimaryKey), TtlSettings: proto.CloneOf(observed.TtlSettings),
		StorageSettings: declaredStorage(observed.StorageSettings), PartitioningSettings: proto.CloneOf(observed.PartitioningSettings),
		KeyBloomFilter: observed.KeyBloomFilter, ReadReplicasSettings: proto.CloneOf(observed.ReadReplicasSettings), Tiering: observed.Tiering, Temporary: observed.Temporary,
	}
	for _, family := range observed.ColumnFamilies {
		result.ColumnFamilies = append(result.ColumnFamilies, proto.CloneOf(family))
	}
	result.Attributes = maps.Clone(observed.Attributes)
	result.Access = accessIntent(observed.Self)
	return result
}

// WithoutNewTableDefaults is intent without each setting that has the value
// YDB's documentation gives a new table, the baseline the host's embedded
// engine wrote a read's report against: splitting by size at 2048 MB, not by
// load, at least one partition, and no key Bloom filter. A read can report
// those defaults explicitly, and a setting a declaration leaves out keeps
// what the table holds, so leaving them out changes no forward plan. A reverse
// states them (see restoredTable). intent is not changed.
func WithoutNewTableDefaults(intent *native.RowTableIntent) *native.RowTableIntent {
	result := proto.CloneOf(intent)
	if partitioning := result.GetPartitioningSettings(); partitioning != nil {
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
		if proto.Equal(partitioning, &Ydb_Table.PartitioningSettings{}) {
			result.PartitioningSettings = nil
		}
	}
	if result.GetKeyBloomFilter() == Ydb.FeatureFlag_DISABLED {
		result.KeyBloomFilter = Ydb.FeatureFlag_STATUS_UNSPECIFIED
	}
	return result
}

// defaultPartitionSizeMB is the partition size YDB's documentation gives a new
// table that splits by size.
const defaultPartitionSizeMB = 2048

// tableObservation writes a desired table as a read of it would report it. A
// starting layout is create-time only and YDB keeps no record of it, so a read
// finds none and the conversion leaves it out.
func tableObservation(intent *native.RowTableIntent) *native.DescribeTableResult {
	result := &native.DescribeTableResult{PrimaryKey: slices.Clone(intent.PrimaryKey), TtlSettings: proto.CloneOf(intent.TtlSettings),
		StorageSettings: proto.CloneOf(intent.StorageSettings), PartitioningSettings: proto.CloneOf(intent.PartitioningSettings),
		KeyBloomFilter: intent.KeyBloomFilter, ReadReplicasSettings: proto.CloneOf(intent.ReadReplicasSettings), Tiering: intent.Tiering, Temporary: intent.Temporary, StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW,
	}
	for _, family := range intent.ColumnFamilies {
		result.ColumnFamilies = append(result.ColumnFamilies, proto.CloneOf(family))
	}
	result.Attributes = maps.Clone(intent.Attributes)
	result.Self = accessObservation(intent.Access)
	// YDB keeps no record of a starting layout, only the minimum partition
	// count it sets from one where the declaration states no minimum of its
	// own, so the table a creation leaves reads that minimum.
	if started := startingPartitions(intent); started != 0 && intent.GetPartitioningSettings().GetMinPartitionsCount() == 0 {
		if result.PartitioningSettings == nil {
			result.PartitioningSettings = &Ydb_Table.PartitioningSettings{}
		}
		result.PartitioningSettings.MinPartitionsCount = started
	}
	return result
}

// startingPartitions is how many partitions the starting layout a declaration
// states creates the table with, 0 for none: the uniform count, or one more
// than the split points.
func startingPartitions(intent *native.RowTableIntent) uint64 {
	if points := len(intent.GetPartitionAtKeys()); points != 0 {
		return uint64(points) + 1
	}
	return intent.GetUniformPartitions()
}

// declaredStorage is the storage settings a read reports, as a declaration
// states them. YDB reports store_external_blobs as DISABLED for every row
// table that sets nothing, which is the server's default; as a declaration it
// is no setting, and a creation written from the read needs none.
func declaredStorage(settings *Ydb_Table.StorageSettings) *Ydb_Table.StorageSettings {
	if settings == nil || proto.Equal(settings, &Ydb_Table.StorageSettings{StoreExternalBlobs: Ydb.FeatureFlag_DISABLED}) {
		return nil
	}
	return proto.CloneOf(settings)
}
