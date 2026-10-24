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

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
		"starting_layout":  "uniform_partitions is a count of at least 1; partition_at_keys lists split points of source text values; at most one of them; YDB takes a layout only when it creates a table and keeps no record of it, so it is never observed",
		"access":           "present access declares the complete direct-grant set and requires the complete effective-permission set; owner omission leaves ownership unmanaged; constraints never grant permissions or infer inheritance settings",
		"omission":         "missing settings request native defaults and remain absent during normalization; missing access leaves access unmanaged",
		"primary_key":      "a declaration that states settings only, such as a YAML or Go source, omits the key and the effective key comes from the common columns; a stated key is distinct, ordered, and must agree with them",
		"runtime_state":    "physical shard boundaries refuse conversion; statistics and transient identity are not allowed in either property model",
		"row_storage":      "this model requests row storage; observed unspecified storage is interpreted as row storage",
		"conversion":       "offline representation conversion establishes no observation coverage; permission planning is not advertised",
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
		SourceFormats: []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS}}
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
	families := map[string]bool{}
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
	for _, point := range intent.PartitionAtKeys {
		if len(point.GetValues()) == 0 {
			return fmt.Errorf("a split point of partition_at_keys holds no value")
		}
		for _, value := range point.Values {
			if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
				return fmt.Errorf("a partition_at_keys value is not valid UTF-8 without NUL")
			}
		}
	}
	if ttl := intent.TtlSettings; ttl != nil {
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
	}
	return validateAccessIntent(intent.Access)
}

func validatePermissionSet(grants []*Ydb_Scheme.Permissions) error {
	subjects := map[string]bool{}
	for _, grant := range grants {
		if grant.GetSubject() == "" || subjects[grant.Subject] || !distinctColumns(grant.PermissionNames) {
			return fmt.Errorf("invalid or duplicate YDB permission subject or names")
		}
		subjects[grant.Subject] = true
	}
	return nil
}

// ConvertTable changes row-table representations without treating physical
// topology as creation intent or inherited permissions as direct grants.
func ConvertTable(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
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
		return convertedEnvelope(value, TableDesiredModel(), tableIntent(&observed), NormalizeDesired)
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
		StorageSettings: proto.CloneOf(observed.StorageSettings), PartitioningSettings: proto.CloneOf(observed.PartitioningSettings),
		KeyBloomFilter: observed.KeyBloomFilter, ReadReplicasSettings: proto.CloneOf(observed.ReadReplicasSettings), Tiering: observed.Tiering, Temporary: observed.Temporary,
	}
	for _, family := range observed.ColumnFamilies {
		result.ColumnFamilies = append(result.ColumnFamilies, proto.CloneOf(family))
	}
	result.Attributes = maps.Clone(observed.Attributes)
	result.Access = accessIntent(observed.Self)
	return result
}

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
	return result
}
