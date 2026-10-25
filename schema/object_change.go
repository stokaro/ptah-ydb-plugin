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

// objectChanges describes the change and operation models of one kind of
// standalone object: complete before and after operands, either absent, and
// the safety facts the object's family derives from them. before is the
// object's observed model and after its desired model.
type objectChanges struct {
	kind, change, operation string
	noun                    string
	// semantics derives a change's safety facts from its normalized
	// operands, either nil, and refuses a pair that holds no change.
	semantics func(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error)
	// definition is what the change model says beyond its operands, and
	// execution what the operation model says about how it runs.
	definition, execution map[string]any
	changeModel           *protocol.ValueModel
	operationModel        *protocol.ValueModel
}

func newObjectChanges(c objectChanges) *objectChanges {
	payload := map[string]any{"required": []string{"before", "after"}, "additionalProperties": false,
		"before": map[string]any{"nullable": true, "model": json.RawMessage(model(c.kind).DefinitionJson)},
		"after":  map[string]any{"nullable": true, "model": json.RawMessage(NativeDesiredModel(c.kind).DefinitionJson)}}
	definition := map[string]any{"kind": c.change, "representation": "change", "schema_version": 1, "payload": payload,
		"operands": "before is the complete native observation including access metadata; after is complete desired intent; null means established absence; both null refused"}
	for key, value := range c.definition {
		definition[key] = value
	}
	c.changeModel = buildJSONModel(c.change, protocol.ValueInput_CHANGE, definition)
	operation := map[string]any{"kind": c.operation, "representation": "operation", "schema_version": 1, "payload": json.RawMessage(c.changeModel.DefinitionJson)}
	for key, value := range c.execution {
		operation[key] = value
	}
	c.operationModel = buildJSONModel(c.operation, protocol.ValueInput_OPERATION, operation)
	return &c
}

func buildJSONModel(kind string, representation protocol.ValueInput_Representation, definition map[string]any) *protocol.ValueModel {
	data, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(data)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: kind, Representation: representation, SchemaVersion: 1, Encoding: "json", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: kind + "/json-v1"}
}

// capture builds a change from complete operands under one owner. Callers
// establish absence and coverage before passing a nil operand.
func (c *objectChanges) capture(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	operands := map[string]json.RawMessage{"before": json.RawMessage("null"), "after": json.RawMessage("null")}
	for _, entry := range []struct {
		key   string
		value *protocol.Envelope
	}{{"before", before}, {"after", after}} {
		if entry.value == nil {
			continue
		}
		if !proto.Equal(owner, entry.value.Owner) {
			return nil, fmt.Errorf("%s change operands have different owners", c.noun)
		}
		operands[entry.key] = entry.value.Payload
	}
	payload, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	return c.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: c.change, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: c.changeModel.Encoding, DefinitionSha256: c.changeModel.DefinitionSha256, Payload: payload}})
}

// normalizeChange reconstructs a change's safety facts from its complete
// operands. A caller cannot supply removal flags or impact in the payload.
func (c *objectChanges) normalizeChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value := input.GetValue()
	if input.GetRepresentation() != protocol.ValueInput_CHANGE || value.GetProviderId() != ProviderID || value.GetKind() != c.change || value.GetSchemaVersion() != c.changeModel.SchemaVersion ||
		value.GetEncoding() != c.changeModel.Encoding || value.GetDefinitionSha256() != c.changeModel.DefinitionSha256 {
		return nil, fmt.Errorf("%s change does not match its negotiated definition", c.noun)
	}
	before, after, err := c.operands(value)
	if err != nil {
		return nil, err
	}
	semantics, err := c.semantics(before, after)
	if err != nil {
		return nil, err
	}
	operands := map[string]json.RawMessage{"before": json.RawMessage("null"), "after": json.RawMessage("null")}
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
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: c.changeModel.CanonicalFormat, CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}

// operands returns a change's operands, each normalized under the change's
// owner, or nil for an absent one. Both absent is refused.
func (c *objectChanges) operands(value *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &payload); err != nil || len(payload) != 2 || payload["before"] == nil || payload["after"] == nil {
		return nil, nil, fmt.Errorf("%s change requires exact before and after operands", c.noun)
	}
	operand := func(key string, definition *protocol.ValueModel, normalize func(*protocol.Envelope) (*protocol.Envelope, error)) (*protocol.Envelope, error) {
		if bytes.Equal(payload[key], []byte("null")) {
			return nil, nil
		}
		return normalize(&protocol.Envelope{ProviderId: ProviderID, Kind: c.kind, Owner: proto.CloneOf(value.Owner), SchemaVersion: definition.SchemaVersion,
			Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(payload[key])})
	}
	before, err := operand("before", model(c.kind), Normalize)
	if err != nil {
		return nil, nil, err
	}
	after, err := operand("after", NativeDesiredModel(c.kind), NormalizeDesired)
	if err != nil {
		return nil, nil, err
	}
	if before == nil && after == nil {
		return nil, nil, fmt.Errorf("%s change has no operands", c.noun)
	}
	return before, after, nil
}

// operationFromChange lowers a complete change without losing its operands.
func (c *objectChanges) operationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	if _, err := c.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: change}); err != nil {
		return nil, err
	}
	value := proto.CloneOf(change)
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = c.operation, c.operationModel.Encoding, c.operationModel.SchemaVersion, c.operationModel.DefinitionSha256
	return c.normalizeOperation(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: value})
}

// normalizeOperation reconstructs an operation's safety facts from the
// change it carries.
func (c *objectChanges) normalizeOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value := input.GetValue()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != ProviderID || value.GetKind() != c.operation || value.GetSchemaVersion() != c.operationModel.SchemaVersion ||
		value.GetEncoding() != c.operationModel.Encoding || value.GetDefinitionSha256() != c.operationModel.DefinitionSha256 {
		return nil, fmt.Errorf("%s operation does not match its negotiated definition", c.noun)
	}
	captured, err := c.normalizeChange(&protocol.ValueInput{Id: input.Id, Representation: protocol.ValueInput_CHANGE, Value: c.operationChange(value)})
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = bytes.Clone(captured.Value.Payload)
	facts := captured.ChangeSemantics
	return &protocol.NormalizedValue{Id: input.Id, Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: c.operationModel.CanonicalFormat, CanonicalProjection: bytes.Clone(result.Payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: facts.Impact, Reason: facts.Reason, Access: facts.Access, AccessReason: facts.AccessReason}}, nil
}

// operationChange restores the change envelope an operation carries.
func (c *objectChanges) operationChange(operation *protocol.Envelope) *protocol.Envelope {
	value := proto.CloneOf(operation)
	if value == nil {
		return nil
	}
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = c.change, c.changeModel.Encoding, c.changeModel.SchemaVersion, c.changeModel.DefinitionSha256
	return value
}
