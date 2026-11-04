package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// IndexSettingsOperationKind identifies a change of the automatic
// partitioning settings and the read replicas of one global index of an
// existing row table.
const IndexSettingsOperationKind = schema.ProviderID + "/row-table-index-settings-operation"

var indexSettingsModel = buildIndexSettingsModel()

// IndexSettingsModel binds the index settings payload grammar, its
// precondition and its confirmation.
func IndexSettingsModel() *protocol.ValueModel { return proto.CloneOf(indexSettingsModel) }

func buildIndexSettingsModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": IndexSettingsOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"payload": "index: the index name; set: the settings that change, as a native GlobalIndexSettings with partitioning_settings and read_replicas_settings only; " +
			"held_index: the index as the table held it, a global sync, async or unique index with its settings",
		"partitioning": "partitioning_settings states the settings that change among the automatic partitioning by size and by load, the preferred partition size " +
			"and the minimum and maximum partition counts; the partition columns are never named, a setting is never reset, and the minimum never exceeds a nonzero maximum",
		"replicas":     "read_replicas_settings states PER_AZ or ANY_AZ replicas where they change, a count of zero removing them",
		"execution":    "YQL ALTER TABLE ... ALTER INDEX ... SET through the query service once, outside SQL transactions, naming every partitioning setting the index ends with when the partitioning changes; no retries; session and absolute path bound at dispatch",
		"precondition": "the table must exist as a row table and hold the index exactly as captured, settings included",
		"confirmation": "read the table back; the index must keep its definition and be ready, hold every partitioning setting stated and the partition columns it held, and the read replicas set, or else the ones it held; every other index, every column and the key must be unchanged",
		"semantics":    "behavioral: the settings decide when YDB splits and merges the index's partitions and where it serves reads; no value is rewritten",
		"database":     "an explicit owner database must match the selected session; an empty database binds to that session",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: IndexSettingsOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: IndexSettingsOperationKind + "/json-v1"}
}

// indexSettingsChange is a change of the partitioning and the read replicas
// of one global index both sides hold with one definition.
type indexSettingsChange struct {
	// held is the index as the table holds it, settings included.
	held *native.TableIndexDescription
	// set holds the partitioning settings and the read replicas that change,
	// each nil when it does not.
	set *native.GlobalIndexSettings
}

// IndexSettings is a decoded change of one index's settings.
type IndexSettings struct {
	// Index names the index.
	Index string
	// Set holds the partitioning settings that change, nil when none does, and
	// the read replicas the index takes, nil when they stay.
	Set *native.GlobalIndexSettings
	// Held is the index as the table held it when planned, settings included.
	Held *native.TableIndexDescription
}

// HeldIndexSettings returns the partitioning and read replicas a global
// sync, async or unique index holds, never nil: the server describes them on
// the index, as on the table that implements it. Measured on 25.1.4.7 and
// 26.2.1.14, DescribeTable reports a new index's defaults there, and the
// values an ALTER INDEX sets.
func HeldIndexSettings(index *native.TableIndexDescription) *native.GlobalIndexSettings {
	var settings *native.GlobalIndexSettings
	switch kind := index.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex:
		settings = kind.GlobalIndex.GetSettings()
	case *native.TableIndexDescription_GlobalAsyncIndex:
		settings = kind.GlobalAsyncIndex.GetSettings()
	case *native.TableIndexDescription_GlobalUniqueIndex:
		settings = kind.GlobalUniqueIndex.GetSettings()
	}
	if settings == nil {
		return &native.GlobalIndexSettings{}
	}
	return proto.CloneOf(settings)
}

// planIndexSettings returns the change that gives the index named name the
// partitioning and read replicas want states, or the reason it cannot be
// made. It returns nil when the index the table holds satisfies want. The
// rules are the table's (see planPartitioning): a setting want leaves out
// keeps what the index holds, switching partitioning by size on names every
// setting the index keeps, and replicas declared as none are written as
// `PER_AZ:0`. A starting layout and partition columns are refused by name:
// YDB keys an index's partitions on the index's own key, and takes no layout
// for an index that exists.
func planIndexSettings(name string, want *native.GlobalIndexSettings, held *native.TableIndexDescription) (*indexSettingsChange, string) {
	if want == nil || proto.Size(want) == 0 {
		return nil, ""
	}
	if want.Partitions != nil {
		return nil, fmt.Sprintf("index %q declares a starting layout (uniform_partitions or partition_at_keys), which YDB does not take for an index the table holds", name)
	}
	if len(want.GetPartitioningSettings().GetPartitionBy()) != 0 {
		return nil, fmt.Sprintf("index %q declares partition columns, and YDB partitions an index on its own key", name)
	}
	have := HeldIndexSettings(held)
	partitioning := withSizeSplitting(want.GetPartitioningSettings())
	delta := partitioningDelta(partitioning, have.GetPartitioningSettings())
	if delta != nil {
		if reason := partitioningRefusal(have.GetPartitioningSettings(), delta); reason != "" {
			return nil, fmt.Sprintf("index %q: %s", name, reason)
		}
		if delta.GetPartitioningBySize() == Ydb.FeatureFlag_ENABLED {
			delta = resolvedPartitioning(partitioning, have.GetPartitioningSettings())
		}
	}
	replicas := replicasDelta(want.GetReadReplicasSettings(), have.GetReadReplicasSettings())
	if delta == nil && replicas == nil {
		return nil, ""
	}
	return &indexSettingsChange{held: proto.CloneOf(held), set: &native.GlobalIndexSettings{PartitioningSettings: delta, ReadReplicasSettings: replicas}}, ""
}

type indexSettingsPayload struct {
	Index     string          `json:"index"`
	Set       json.RawMessage `json:"set"`
	HeldIndex json.RawMessage `json:"held_index"`
}

// CaptureIndexSettings builds the operation for a change of one index's
// settings planned against a table in owner.
func CaptureIndexSettings(id string, owner *protocol.Reference, change indexSettingsChange) (*protocol.NormalizedValue, error) {
	encoded, err := encodeIndexSettings(&IndexSettings{Index: change.held.GetName(), Set: change.set, Held: change.held})
	if err != nil {
		return nil, err
	}
	definition := IndexSettingsModel()
	return NormalizeIndexSettings(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: IndexSettingsOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

func encodeIndexSettings(settings *IndexSettings) ([]byte, error) {
	set, err := protojson.Marshal(settings.Set)
	if err != nil {
		return nil, err
	}
	held, err := protojson.Marshal(settings.Held)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(indexSettingsPayload{Index: settings.Index, Set: set, HeldIndex: held})
	if err != nil {
		return nil, err
	}
	return modeljson.Canonical(raw)
}

// NormalizeIndexSettings validates a change of an index's settings and
// reconstructs its safety facts from the payload, never from the sender.
func NormalizeIndexSettings(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), IndexSettingsModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB index settings change does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	settings, err := decodeIndexSettings(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := encodeIndexSettings(settings)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL,
			Reason: "Changes when YDB splits and merges an index's partitions, or its read replicas; no value is rewritten",
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Alteration inherits server access policy"}}, nil
}

// DecodeIndexSettings returns independent native operands after validating
// the envelope.
func DecodeIndexSettings(value *protocol.Envelope) (*IndexSettings, error) {
	normalized, err := NormalizeIndexSettings(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeIndexSettings(normalized.Value.Payload)
}

func decodeIndexSettings(raw []byte) (*IndexSettings, error) {
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return nil, err
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil || containsNull(fields) {
		return nil, fmt.Errorf("YDB index settings cannot be null")
	}
	var payload indexSettingsPayload
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Set == nil || payload.HeldIndex == nil {
		return nil, fmt.Errorf("invalid YDB index settings JSON")
	}
	settings := &IndexSettings{Index: payload.Index, Set: &native.GlobalIndexSettings{}, Held: &native.TableIndexDescription{}}
	if err := protojson.Unmarshal(payload.Set, settings.Set); err != nil {
		return nil, fmt.Errorf("invalid YDB index settings")
	}
	if err := protojson.Unmarshal(payload.HeldIndex, settings.Held); err != nil {
		return nil, fmt.Errorf("invalid held YDB index")
	}
	return settings, validateIndexSettings(settings)
}

// validateIndexSettings checks a change of an index's settings. It names a
// global sync, async or unique index the table held under that name, states
// only partitioning settings and read replicas, and changes at least one of
// them, with the rules a table's partitioning and replicas follow.
func validateIndexSettings(settings *IndexSettings) error {
	if err := schema.Known(settings.Held); err != nil {
		return err
	}
	if !validName(settings.Index) || settings.Held.GetName() != settings.Index {
		return fmt.Errorf("YDB index settings name another index than the one held")
	}
	switch settings.Held.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex, *native.TableIndexDescription_GlobalAsyncIndex, *native.TableIndexDescription_GlobalUniqueIndex:
	default:
		return fmt.Errorf("YDB changes the settings of a global sync, async or unique index only")
	}
	if err := only(settings.Set, "partitioning_settings", "read_replicas_settings"); err != nil {
		return err
	}
	held := HeldIndexSettings(settings.Held)
	if set := settings.Set.GetPartitioningSettings(); set != nil {
		if err := only(set, "partitioning_by_size", "partitioning_by_load", "partition_size_mb", "min_partitions_count", "max_partitions_count"); err != nil {
			return err
		}
		if partitioningDelta(set, held.GetPartitioningSettings()) == nil {
			return fmt.Errorf("YDB index partitioning change changes nothing")
		}
		if reason := partitioningRefusal(held.GetPartitioningSettings(), set); reason != "" {
			return fmt.Errorf("YDB index partitioning change is invalid: %s", reason)
		}
	}
	if set := settings.Set.GetReadReplicasSettings(); set != nil {
		if set.GetSettings() == nil {
			return fmt.Errorf("YDB index read replica change names no mode")
		}
		if SameReplicas(set, held.GetReadReplicasSettings()) {
			return fmt.Errorf("YDB index read replica change changes nothing")
		}
	}
	if settings.Set.GetPartitioningSettings() == nil && settings.Set.GetReadReplicasSettings() == nil {
		return fmt.Errorf("YDB index settings change states no setting")
	}
	return nil
}

// IndexSettingsStatement writes the statement that changes an index's
// settings on the table at path, as the host's embedded engine writes it:
// `ALTER TABLE `t` ALTER INDEX `i` SET (...)`, naming every partitioning
// setting the index ends with when the partitioning changes, since one setting
// can reset another, and the read replicas where they change.
func IndexSettingsStatement(path string, settings *IndexSettings) string {
	var items []string
	if set := settings.Set.GetPartitioningSettings(); set != nil {
		items = splittingActions(HeldIndexSettings(settings.Held).GetPartitioningSettings(), set)
	}
	if replicas := settings.Set.GetReadReplicasSettings(); replicas != nil {
		items = append(items, `READ_REPLICAS_SETTINGS = "`+replicasText(replicas)+`"`)
	}
	return "ALTER TABLE " + QuoteIdentifier(path) + " ALTER INDEX " + QuoteIdentifier(settings.Index) + " SET (" + strings.Join(items, ", ") + ");"
}
