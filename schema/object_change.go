package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// objectChanges describes the change and operation models of one kind of
// standalone object: complete before and after operands, either absent, and
// the safety facts the object's family derives from them. before is the
// object's observed model and after its desired model.
type objectChanges struct {
	// operation is empty for a family whose changes run inside an operation
	// of their own, such as the workload operation.
	kind, change, operation string
	noun                    string
	// semantics derives a change's safety facts from its normalized
	// operands, either nil, and refuses a pair that holds no change.
	semantics func(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error)
	// definition is what the change model says beyond its operands, and
	// execution what the operation model says about how it runs.
	definition, execution map[string]any
	// database makes a change carry the database its operands were read
	// from, beside them, for a family whose operands hold paths a planner
	// reads against it.
	database bool
	// operations, when set, are the ways an operation may carry its change
	// out, one of which every operation names; allowed says which of them a
	// change with the operands present takes.
	operations     []string
	allowed        func(operation string, before, after bool) bool
	changeModel    *protocol.ValueModel
	operationModel *protocol.ValueModel
}

func newObjectChanges(c objectChanges) *objectChanges {
	payload := map[string]any{"required": []string{"before", "after"}, "additionalProperties": false,
		"before": map[string]any{"nullable": true, "model": json.RawMessage(model(c.kind).DefinitionJson)},
		"after":  map[string]any{"nullable": true, "model": json.RawMessage(NativeDesiredModel(c.kind).DefinitionJson)}}
	for _, key := range []string{"before_context", "after_context"} {
		payload[key] = map[string]any{"optional": true, "meaning": "complete database.context receipt for the present operand, independent of its identity"}
	}
	if c.database {
		payload["database"] = map[string]any{"type": "string", "optional": true,
			"meaning": "the absolute path of the database the operands were read from; omitted where the comparison could not name it"}
	}
	definition := map[string]any{"kind": c.change, "representation": "change", "schema_version": 1, "payload": payload,
		"operands": "before is the complete native observation including access metadata; after is complete desired intent; null means established absence; both null refused"}
	for key, value := range c.definition {
		definition[key] = value
	}
	c.changeModel = buildJSONModel(c.change, protocol.ValueInput_CHANGE, definition)
	if c.operation == "" {
		// The family's changes run as part of an operation of its own.
		return &c
	}
	operation := map[string]any{"kind": c.operation, "representation": "operation", "schema_version": 1, "payload": json.RawMessage(c.changeModel.DefinitionJson)}
	if c.operations != nil {
		operation["operation"] = map[string]any{"required": true, "enum": c.operations,
			"meaning": "how the operation carries out the change it holds beside the operation key"}
	}
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
	return c.captureIn(id, owner, "", before, after)
}

// captureIn builds a change whose operands were read from database, "" where
// it is not known.
func (c *objectChanges) captureIn(id string, owner *protocol.Reference, database string, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
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
		if entry.value.Context != nil {
			encoded, err := protojson.Marshal(entry.value.Context)
			if err != nil {
				return nil, err
			}
			operands[entry.key+"_context"] = encoded
		}
	}
	if database != "" {
		if !c.database {
			return nil, fmt.Errorf("%s change carries no database", c.noun)
		}
		encoded, err := json.Marshal(database)
		if err != nil {
			return nil, err
		}
		operands["database"] = encoded
	}
	payload, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	bound, err := operandContext(before, after)
	if err != nil {
		return nil, err
	}
	return c.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: c.change, Owner: proto.CloneOf(owner), Context: bound, SchemaVersion: 1, Encoding: c.changeModel.Encoding, DefinitionSha256: c.changeModel.DefinitionSha256, Payload: payload}})
}

// normalizeChange reconstructs a change's safety facts from its complete
// operands. A caller cannot supply removal flags or impact in the payload.
func (c *objectChanges) normalizeChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value := input.GetValue()
	if input.GetRepresentation() != protocol.ValueInput_CHANGE || value.GetProviderId() != ProviderID || value.GetKind() != c.change || value.GetSchemaVersion() != c.changeModel.SchemaVersion ||
		value.GetEncoding() != c.changeModel.Encoding || value.GetDefinitionSha256() != c.changeModel.DefinitionSha256 {
		return nil, fmt.Errorf("%s change does not match its negotiated definition", c.noun)
	}
	before, after, database, err := c.parts(value)
	if err != nil {
		return nil, err
	}
	semantics, err := c.semantics(before, after)
	if err != nil {
		return nil, err
	}
	canonical, err := c.encode(before, after, database, "")
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: c.changeModel.CanonicalFormat, CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}

// encode writes a change's canonical payload, with the operation an
// operation names when operation is not "".
func (c *objectChanges) encode(before, after *protocol.Envelope, database, operation string) ([]byte, error) {
	operands := map[string]any{"before": json.RawMessage("null"), "after": json.RawMessage("null")}
	if before != nil {
		operands["before"] = json.RawMessage(before.Payload)
	}
	if after != nil {
		operands["after"] = json.RawMessage(after.Payload)
	}
	for key, operand := range map[string]*protocol.Envelope{"before_context": before, "after_context": after} {
		if operand.GetContext() != nil {
			encoded, err := protojson.Marshal(operand.Context)
			if err != nil {
				return nil, err
			}
			operands[key] = json.RawMessage(encoded)
		}
	}
	if database != "" {
		operands["database"] = database
	}
	if operation != "" {
		operands["operation"] = operation
	}
	encoded, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	return modeljson.Canonical(encoded)
}

// operands returns a change's operands, each normalized under the change's
// owner, or nil for an absent one. Both absent is refused.
func (c *objectChanges) operands(value *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	before, after, _, err := c.parts(value)
	return before, after, err
}

// parts returns a change's operands and the database it names, "" for none.
func (c *objectChanges) parts(value *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, string, error) {
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, nil, "", err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &payload); err != nil || payload["before"] == nil || payload["after"] == nil {
		return nil, nil, "", fmt.Errorf("%s change requires exact before and after operands", c.noun)
	}
	var database string
	if encoded, found := payload["database"]; found && c.database {
		if err := json.Unmarshal(encoded, &database); err != nil || database == "" {
			return nil, nil, "", fmt.Errorf("%s change names its database as a nonempty string", c.noun)
		}
		if err := ValidateNativeOwner(&protocol.Reference{Engine: "ydb", Kind: c.kind, Database: database, Name: "x"}); err != nil {
			return nil, nil, "", fmt.Errorf("%s change names an invalid database", c.noun)
		}
		delete(payload, "database")
	}
	contexts := map[string]*protocol.DatabaseContext{}
	for _, key := range []string{"before", "after"} {
		if encoded, found := payload[key+"_context"]; found {
			bound := &protocol.DatabaseContext{}
			if bytes.Equal(payload[key], []byte("null")) || protojson.Unmarshal(encoded, bound) != nil || ValidateDatabaseContext(bound) != nil {
				return nil, nil, "", fmt.Errorf("%s change has invalid operand context", c.noun)
			}
			if database != "" && bound.Database != database {
				return nil, nil, "", fmt.Errorf("%s change context differs from its selected database", c.noun)
			}
			contexts[key] = bound
			delete(payload, key+"_context")
		}
	}
	if len(payload) != 2 {
		return nil, nil, "", fmt.Errorf("%s change requires exact before and after operands", c.noun)
	}
	operand := func(key string, definition *protocol.ValueModel, normalize func(*protocol.Envelope) (*protocol.Envelope, error)) (*protocol.Envelope, error) {
		if bytes.Equal(payload[key], []byte("null")) {
			return nil, nil
		}
		result, err := normalize(&protocol.Envelope{ProviderId: ProviderID, Kind: c.kind, Owner: proto.CloneOf(value.Owner), SchemaVersion: definition.SchemaVersion,
			Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(payload[key]), Context: contexts[key]})
		if err != nil {
			return nil, err
		}
		_, pending, err := RequiredObjects(result)
		if err != nil {
			return nil, err
		}
		if pending {
			return nil, fmt.Errorf("%s change requires resolved operand context", c.noun)
		}
		return result, nil
	}
	before, err := operand("before", model(c.kind), Normalize)
	if err != nil {
		return nil, nil, "", err
	}
	after, err := operand("after", NativeDesiredModel(c.kind), NormalizeDesired)
	if err != nil {
		return nil, nil, "", err
	}
	if before == nil && after == nil {
		return nil, nil, "", fmt.Errorf("%s change has no operands", c.noun)
	}
	bound, err := operandContext(before, after)
	if err != nil {
		return nil, nil, "", err
	}
	if !proto.Equal(value.Context, bound) {
		return nil, nil, "", fmt.Errorf("%s change context differs from its operands", c.noun)
	}
	return before, after, database, nil
}

// The outer receipt lets the host check the dispatch target without reading
// opaque payloads. Each operand keeps its own authored root inside the capture.
func operandContext(before, after *protocol.Envelope) (*protocol.DatabaseContext, error) {
	var result *protocol.DatabaseContext
	for _, operand := range []*protocol.Envelope{before, after} {
		bound := operand.GetContext()
		if bound == nil {
			continue
		}
		if err := ValidateDatabaseContext(bound); err != nil {
			return nil, err
		}
		if result != nil && result.Database != bound.Database {
			return nil, fmt.Errorf("YDB change operands name different selected databases")
		}
		result = proto.CloneOf(bound)
	}
	return result, nil
}

// operationFromChange lowers a complete change without losing its operands.
func (c *objectChanges) operationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return c.operationAs(id, change, "")
}

// operationAs lowers a complete change to an operation that carries it out
// the way operation names, "" for a family whose operations name none.
func (c *objectChanges) operationAs(id string, change *protocol.Envelope, operation string) (*protocol.NormalizedValue, error) {
	captured, err := c.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: change})
	if err != nil {
		return nil, err
	}
	value := proto.CloneOf(captured.Value)
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = c.operation, c.operationModel.Encoding, c.operationModel.SchemaVersion, c.operationModel.DefinitionSha256
	if operation != "" {
		before, after, database, err := c.parts(captured.Value)
		if err != nil {
			return nil, err
		}
		if value.Payload, err = c.encode(before, after, database, operation); err != nil {
			return nil, err
		}
	}
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
	change, operation, err := c.split(value)
	if err != nil {
		return nil, err
	}
	captured, err := c.normalizeChange(&protocol.ValueInput{Id: input.Id, Representation: protocol.ValueInput_CHANGE, Value: change})
	if err != nil {
		return nil, err
	}
	payload := bytes.Clone(captured.Value.Payload)
	if c.operations != nil {
		before, after, database, err := c.parts(captured.Value)
		if err != nil {
			return nil, err
		}
		if !c.allowed(operation, before != nil, after != nil) {
			return nil, fmt.Errorf("%s operation %q cannot carry out its change", c.noun, operation)
		}
		if payload, err = c.encode(before, after, database, operation); err != nil {
			return nil, err
		}
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	facts := captured.ChangeSemantics
	return &protocol.NormalizedValue{Id: input.Id, Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: c.operationModel.CanonicalFormat, CanonicalProjection: bytes.Clone(result.Payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: facts.Impact, Reason: facts.Reason, Access: facts.Access, AccessReason: facts.AccessReason}}, nil
}

// split separates an operation into the change it carries and the
// operation it names, "" for a family whose operations name none.
func (c *objectChanges) split(operation *protocol.Envelope) (*protocol.Envelope, string, error) {
	change := c.operationChange(operation)
	if c.operations == nil {
		return change, "", nil
	}
	canonical, err := modeljson.Canonical(operation.GetPayload())
	if err != nil {
		return nil, "", err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &payload); err != nil {
		return nil, "", fmt.Errorf("%s operation requires an object", c.noun)
	}
	var name string
	if err := json.Unmarshal(payload["operation"], &name); err != nil || !slices.Contains(c.operations, name) {
		return nil, "", fmt.Errorf("%s operation names none of %v", c.noun, c.operations)
	}
	delete(payload, "operation")
	if change.Payload, err = json.Marshal(payload); err != nil {
		return nil, "", err
	}
	return change, name, nil
}

// operationChange restores the change envelope an operation carries, its
// payload still naming the operation for a family whose operations name one.
func (c *objectChanges) operationChange(operation *protocol.Envelope) *protocol.Envelope {
	value := proto.CloneOf(operation)
	if value == nil {
		return nil
	}
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = c.change, c.changeModel.Encoding, c.changeModel.SchemaVersion, c.changeModel.DefinitionSha256
	return value
}
