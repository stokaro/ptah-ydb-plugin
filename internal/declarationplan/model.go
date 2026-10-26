// Package declarationplan creates YDB objects from authored definitions without
// asserting that a live catalog contains or lacks them.
package declarationplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the authored native-object planner.
const ServiceID = "ydb-authored-objects"

// OperationKind names an authored CREATE, with no observed operand.
const OperationKind = schema.ProviderID + "/authored-create"

// Kinds are the desired standalone models this service creates.
func Kinds() []string {
	return []string{schema.CoordinationProperties, schema.TopicProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties, schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties}
}

// Model declares the desired-only CREATE operation and its text execution form.
func Model() *protocol.ValueModel {
	definition := []byte(`{"access":"creation inherits database access; explicit access requirements have no text form","execution":"render one CREATE statement; the server checks existence during dispatch; SQL units only; no automatic retry","kind":"github.com/stokaro/ptah-ydb-plugin/authored-create","payload":"one complete normalized desired envelope; no current state"}`)
	sum := sha256.Sum256(definition)
	return &protocol.ValueModel{Kind: OperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json", DefinitionJson: definition, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: OperationKind + "/json-v1"}
}

type payload struct {
	Desired *protocol.Envelope `json:"desired"`
}

// Capture freezes one authored declaration as a CREATE operation.
func Capture(id string, desired *protocol.Envelope) (*protocol.NormalizedValue, error) {
	raw, err := json.Marshal(payload{Desired: desired})
	if err != nil {
		return nil, err
	}
	model := Model()
	return Normalize(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(desired.GetOwner()), Payload: raw}})
}

// Normalize validates the complete desired definition and derives CREATE facts.
func Normalize(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, model := input.GetValue(), Model()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != model.Kind || value.GetSchemaVersion() != model.SchemaVersion || value.GetEncoding() != model.Encoding || value.GetDefinitionSha256() != model.DefinitionSha256 {
		return nil, fmt.Errorf("authored CREATE does not match its declared model")
	}
	desired, err := Declaration(value)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload{Desired: desired})
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return nil, err
	}
	frozen := proto.CloneOf(value)
	frozen.Payload = canonical
	return &protocol.NormalizedValue{Id: input.Id, Value: frozen, NormalizedOwner: proto.CloneOf(desired.Owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(canonical), OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Create the authored object", Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Creation inherits access from the database"}}, nil
}

// Declaration returns the validated desired operand of an authored CREATE.
func Declaration(value *protocol.Envelope) (*protocol.Envelope, error) {
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, err
	}
	var decoded payload
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("invalid authored CREATE: %w", err)
	}
	if !slices.Contains(Kinds(), decoded.Desired.GetKind()) {
		return nil, fmt.Errorf("unsupported authored object kind")
	}
	desired, err := schema.NormalizeDesired(decoded.Desired)
	if err != nil {
		return nil, err
	}
	if !proto.Equal(value.GetOwner(), desired.GetOwner()) {
		return nil, fmt.Errorf("authored CREATE owner differs from its declaration")
	}
	return desired, nil
}
