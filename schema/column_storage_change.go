package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// ColumnStorageChange identifies a difference between the storage a
// declaration asks for and the storage an existing table holds: another hash
// key, shard count, TTL or local index set of a column table, or a table that
// is a row table on one side and a column table on the other.
const ColumnStorageChange = modelNamespace + "column-storage-change"

// ColumnStorageChangeModel binds the operands of one column-storage change.
func ColumnStorageChangeModel() *protocol.ValueModel { return proto.CloneOf(columnStorageChangeModel) }

var columnStorageChangeModel = buildColumnStorageChangeModel()

func buildColumnStorageChangeModel() *protocol.ValueModel {
	definition, err := json.Marshal(map[string]any{
		"kind": ColumnStorageChange, "representation": "change", "schema_version": 1,
		"payload": map[string]any{"additionalProperties": false,
			"before": json.RawMessage(model(ColumnStorageProperties).DefinitionJson),
			"after":  json.RawMessage(NativeDesiredModel(ColumnStorageProperties).DefinitionJson)},
		"operands":   "the owner is a table; before is the complete observed column storage, omitted when the table is a row table; after is the complete desired column storage, omitted when the declaration asks for a row table; at least one is present and neither is null",
		"semantics":  "behavioral; YDB fixes a table's storage kind, hash key and shard count when it creates the table, so only the TTL changes in place",
		"validation": "exact lowercase keys; each operand is validated against its model",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(definition)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: ColumnStorageChange, Representation: protocol.ValueInput_CHANGE, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: ColumnStorageChange + "/json-v1"}
}

// CaptureColumnStorageChange retains the observed and the desired column
// storage of one table. A nil operand is row storage on that side; at least one
// operand is required, and both must belong to owner.
func CaptureColumnStorageChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	if before == nil && after == nil {
		return nil, fmt.Errorf("column storage change requires an operand")
	}
	operands := map[string]json.RawMessage{}
	for key, operand := range map[string]*protocol.Envelope{"before": before, "after": after} {
		if operand == nil {
			continue
		}
		if !proto.Equal(owner, operand.GetOwner()) || operand.GetKind() != ColumnStorageProperties {
			return nil, fmt.Errorf("column storage change operands belong to another owner or model")
		}
		operands[key] = operand.Payload
	}
	payload, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	contract := ColumnStorageChangeModel()
	return NormalizeColumnStorageChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: contract.Kind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: contract.Encoding,
		DefinitionSha256: contract.DefinitionSha256, Payload: payload}})
}

// ColumnStorageOperands returns the normalized observed and desired operands
// of a change, nil for row storage on that side.
func ColumnStorageOperands(value *protocol.Envelope) (before, after *protocol.Envelope, err error) {
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, nil, err
	}
	var operands map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &operands); err != nil || operands == nil {
		return nil, nil, fmt.Errorf("column storage change requires an object")
	}
	if len(operands) == 0 {
		return nil, nil, fmt.Errorf("column storage change requires an operand")
	}
	for key, raw := range operands {
		if key != "before" && key != "after" {
			return nil, nil, fmt.Errorf("column storage change has an unknown key")
		}
		if bytes.Equal(raw, []byte("null")) {
			return nil, nil, fmt.Errorf("column storage change operands cannot be null")
		}
		definition, normalize := model(ColumnStorageProperties), Normalize
		if key == "after" {
			definition, normalize = NativeDesiredModel(ColumnStorageProperties), NormalizeDesired
		}
		normalized, err := normalize(&protocol.Envelope{ProviderId: ProviderID, Kind: ColumnStorageProperties, Owner: proto.CloneOf(value.GetOwner()), SchemaVersion: 1,
			Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: raw})
		if err != nil {
			return nil, nil, err
		}
		if key == "before" {
			before = normalized
		} else {
			after = normalized
		}
	}
	return before, after, nil
}

// NormalizeColumnStorageChange validates the operands against their models and
// reconstructs the safety facts. A caller cannot supply them in the payload.
func NormalizeColumnStorageChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, contract := input.GetValue(), ColumnStorageChangeModel()
	if input.GetRepresentation() != protocol.ValueInput_CHANGE || value.GetProviderId() != ProviderID || value.GetKind() != contract.Kind ||
		value.GetSchemaVersion() != contract.SchemaVersion || value.GetEncoding() != contract.Encoding || value.GetDefinitionSha256() != contract.DefinitionSha256 {
		return nil, fmt.Errorf("column storage change does not match its negotiated definition")
	}
	if value.GetOwner().GetEngine() != "ydb" || value.GetOwner().GetKind() != "table" {
		return nil, fmt.Errorf("column storage change requires a table owner")
	}
	before, after, err := ColumnStorageOperands(value)
	if err != nil {
		return nil, err
	}
	operands := map[string]json.RawMessage{}
	if before != nil {
		operands["before"] = before.Payload
	}
	if after != nil {
		operands["after"] = after.Payload
	}
	encoded, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(encoded)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "The table's storage differs from its declaration: YDB fixes the storage kind, the hash key and the shard count when it creates a table, so only the TTL changes in place",
		Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Altering table settings preserves native access metadata"}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: contract.CanonicalFormat,
		CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}
