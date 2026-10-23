package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// TablePropertiesChange identifies a difference between the table or column
// properties a declaration states and the ones an existing table holds.
const TablePropertiesChange = modelNamespace + "row-table-properties-change"

// TablePropertiesChangeModel binds the complete operands of one changed table
// or column facet and the safety facts derived from them.
func TablePropertiesChangeModel() *protocol.ValueModel {
	return proto.CloneOf(tablePropertiesChangeModel)
}

var tablePropertiesChangeModel = buildTablePropertiesChangeModel()

func buildTablePropertiesChangeModel() *protocol.ValueModel {
	definition, err := json.Marshal(map[string]any{
		"kind": TablePropertiesChange, "representation": "change", "schema_version": 1,
		"payload": map[string]any{"required": []string{"before", "after"}, "additionalProperties": false,
			"table": map[string]any{
				"before": json.RawMessage(model(TableProperties).DefinitionJson),
				"after":  json.RawMessage(TableDesiredModel().DefinitionJson)},
			"column": map[string]any{
				"before": json.RawMessage(model(ColumnProperties).DefinitionJson),
				"after":  json.RawMessage(ColumnDesiredModel().DefinitionJson)},
			"indexes": map[string]any{
				"facet":  "the kind of the index set model, present exactly when the operands are index sets",
				"before": json.RawMessage(model(IndexProperties).DefinitionJson),
				"after":  json.RawMessage(NativeDesiredModel(IndexProperties).DefinitionJson)}},
		"operands":   "the owner is a table or one of its columns; before is the complete observed facet of that owner and after is the complete desired facet; neither may be null; the facet key names the index set model when the owner is a table and the operands are its index sets",
		"validation": "exact lowercase keys; validate both operands against the model that matches the owner kind, or the index set model when the facet key names it",
		"semantics":  "behavioral; replaces no owner, removes no state, leaves access unchanged",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(definition)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: TablePropertiesChange, Representation: protocol.ValueInput_CHANGE, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: TablePropertiesChange + "/json-v1"}
}

// facetKey names the payload key that selects the index set operands of a table.
const facetKey = "facet"

// tableChangeOperandKind returns the facet kind a change owner carries.
func tableChangeOperandKind(owner *protocol.Reference) (string, error) {
	if owner.GetEngine() != "ydb" {
		return "", fmt.Errorf("table properties change has a foreign owner")
	}
	switch owner.GetKind() {
	case "table":
		return TableProperties, nil
	case "column":
		return ColumnProperties, nil
	default:
		return "", fmt.Errorf("table properties change requires a table or column owner")
	}
}

// CaptureTablePropertiesChange retains the observed and desired facets of one
// owner. Both operands are required, must belong to that owner and must be
// facets of one model: the table or column properties, or the index set of a
// table.
func CaptureTablePropertiesChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	if before == nil || after == nil {
		return nil, fmt.Errorf("table properties change requires both operands")
	}
	for _, operand := range []*protocol.Envelope{before, after} {
		if !proto.Equal(owner, operand.GetOwner()) {
			return nil, fmt.Errorf("table properties change operands have different owners")
		}
	}
	if before.Kind != after.Kind {
		return nil, fmt.Errorf("table properties change operands are facets of different models")
	}
	operands := map[string]json.RawMessage{"before": before.Payload, "after": after.Payload}
	if before.Kind == IndexProperties {
		operands[facetKey] = json.RawMessage(strconv.Quote(IndexProperties))
	}
	payload, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	model := TablePropertiesChangeModel()
	return NormalizeTablePropertiesChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: model.Kind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Payload: payload}})
}

// NormalizeTablePropertiesChange validates both operands against their models
// and reconstructs the safety facts. A caller cannot supply them in the payload.
func NormalizeTablePropertiesChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, contract := input.GetValue(), TablePropertiesChangeModel()
	if input.GetRepresentation() != protocol.ValueInput_CHANGE || value.GetProviderId() != ProviderID || value.GetKind() != contract.Kind ||
		value.GetSchemaVersion() != contract.SchemaVersion || value.GetEncoding() != contract.Encoding || value.GetDefinitionSha256() != contract.DefinitionSha256 {
		return nil, fmt.Errorf("table properties change does not match its negotiated definition")
	}
	operandKind, err := tableChangeOperandKind(value.GetOwner())
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.Payload)
	if err != nil {
		return nil, err
	}
	var operands map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &operands); err != nil || operands["before"] == nil || operands["after"] == nil {
		return nil, fmt.Errorf("table properties change requires exact before and after operands")
	}
	expected := 2
	if selector, found := operands[facetKey]; found {
		expected = 3
		if operandKind != TableProperties || !bytes.Equal(selector, []byte(strconv.Quote(IndexProperties))) {
			return nil, fmt.Errorf("table properties change names an unsupported facet")
		}
		operandKind = IndexProperties
	}
	if len(operands) != expected {
		return nil, fmt.Errorf("table properties change requires exact before and after operands")
	}
	for _, key := range []string{"before", "after"} {
		if bytes.Equal(operands[key], []byte("null")) {
			return nil, fmt.Errorf("table properties change requires both operands")
		}
		definition, normalize := model(operandKind), Normalize
		if key == "after" {
			definition, normalize = desiredModel(operandKind), NormalizeDesired
		}
		normalized, err := normalize(&protocol.Envelope{ProviderId: ProviderID, Kind: operandKind, Owner: proto.CloneOf(value.Owner), SchemaVersion: 1,
			Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: operands[key]})
		if err != nil {
			return nil, err
		}
		operands[key] = normalized.Payload
	}
	encoded, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	canonical, err = modeljson.Canonical(encoded)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	reason := "Table TTL, partitioning and column families change how rows are stored and expired"
	switch operandKind {
	case ColumnProperties:
		reason = "A column moves to another column family"
	case IndexProperties:
		reason = "A secondary index is added, dropped or differs from its declaration; YDB builds a new index in the background and cannot alter one in place"
	}
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: reason,
		Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Altering table settings preserves native access metadata"}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: contract.CanonicalFormat,
		CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}

func desiredModel(kind string) *protocol.ValueModel {
	switch kind {
	case ColumnProperties:
		return ColumnDesiredModel()
	case IndexProperties:
		return NativeDesiredModel(IndexProperties)
	}
	return TableDesiredModel()
}
