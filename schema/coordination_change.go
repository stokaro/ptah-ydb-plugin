package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// CoordinationChange identifies a complete native before/desired after change.
const CoordinationChange = "ydb.coordination-node-change"

// CoordinationSettings is the effective configuration used for semantic equality.
// Source provenance and access metadata are not writable coordination settings.
type CoordinationSettings struct {
	// SelfCheckMillis is the effective self-check interval.
	SelfCheckMillis uint32
	// SessionGraceMillis is the effective grace interval.
	SessionGraceMillis uint32
	// ReadMode is the effective read consistency.
	ReadMode string
	// AttachMode is the effective attachment consistency.
	AttachMode string
	// CountersMode is the effective rate-limiter counter mode.
	CountersMode string
}

// EffectiveCoordination resolves omission only for comparison and execution.
func EffectiveCoordination(value CoordinationIntent) (CoordinationSettings, error) {
	if err := ValidateCoordinationIntent(value); err != nil {
		return CoordinationSettings{}, err
	}
	result := CoordinationSettings{1000, 10000, "relaxed", "strict", "aggregated"}
	if value.SelfCheckPeriodMillis != nil {
		result.SelfCheckMillis = *value.SelfCheckPeriodMillis
	}
	if value.SessionGracePeriodMillis != nil {
		result.SessionGraceMillis = *value.SessionGracePeriodMillis
	}
	if value.ReadConsistencyMode != nil {
		result.ReadMode = *value.ReadConsistencyMode
	}
	if value.AttachConsistencyMode != nil {
		result.AttachMode = *value.AttachConsistencyMode
	}
	if value.RateLimiterCountersMode != nil {
		result.CountersMode = *value.RateLimiterCountersMode
	}
	return result, nil
}

// CoordinationConfiguration extracts comparable settings from a validated model.
// Native access metadata remains in the original observation and change payload.
// Raw values that the server clamps are refused rather than compared as defaults.
func CoordinationConfiguration(value *protocol.Envelope, representation protocol.ValueInput_Representation) (CoordinationIntent, error) {
	var result CoordinationIntent
	if representation == protocol.ValueInput_DESIRED {
		normalized, err := NormalizeDesired(value)
		if err != nil {
			return result, err
		}
		err = json.Unmarshal(normalized.Payload, &result)
		return result, err
	}
	if representation != protocol.ValueInput_OBSERVED || value.GetKind() != CoordinationProperties {
		return result, fmt.Errorf("expected a coordination schema model")
	}
	normalized, err := Normalize(value)
	if err != nil {
		return result, err
	}
	var observed Ydb_Coordination.DescribeNodeResult
	if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
		return result, err
	}
	config := observed.Config
	if config.SelfCheckPeriodMillis != 0 {
		result.SelfCheckPeriodMillis = new(config.SelfCheckPeriodMillis)
	}
	if config.SessionGracePeriodMillis != 0 {
		result.SessionGracePeriodMillis = new(config.SessionGracePeriodMillis)
	}
	for _, mode := range []struct {
		native Ydb_Coordination.ConsistencyMode
		dest   **string
	}{{config.ReadConsistencyMode, &result.ReadConsistencyMode}, {config.AttachConsistencyMode, &result.AttachConsistencyMode}} {
		switch mode.native {
		case Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT:
			*mode.dest = new("strict")
		case Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED:
			*mode.dest = new("relaxed")
		}
	}
	switch config.RateLimiterCountersMode {
	case Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_AGGREGATED:
		result.RateLimiterCountersMode = new("aggregated")
	case Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED:
		result.RateLimiterCountersMode = new("detailed")
	}
	return result, ValidateCoordinationIntent(result)
}

// CoordinationChangeModel binds complete operands and their safety semantics.
func CoordinationChangeModel() *protocol.ValueModel { return proto.CloneOf(coordinationChangeModel) }

var coordinationChangeModel = buildCoordinationChangeModel()

func buildCoordinationChangeModel() *protocol.ValueModel {
	definition, err := json.Marshal(map[string]any{
		"kind": CoordinationChange, "representation": "change", "schema_version": 1,
		"payload": map[string]any{"required": []string{"before", "after"}, "additionalProperties": false,
			"before": map[string]any{"nullable": true, "model": json.RawMessage(model(CoordinationProperties).DefinitionJson)},
			"after":  map[string]any{"nullable": true, "model": json.RawMessage(CoordinationDesiredModel().DefinitionJson)}},
		"operands":   "before is the complete native observation including access metadata; after is complete desired intent; null means established absence; both null refused",
		"validation": "exact lowercase keys; validate both models; reject observed settings subject to server clamping; root ptah_locks is not managed",
		"semantics":  "create: additive, access unknown; alter: behavioral, access unchanged; drop: destructive, removes state, narrows access; never replaces owner",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(definition)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: CoordinationChange, Representation: protocol.ValueInput_CHANGE, SchemaVersion: 1, Encoding: "json", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: CoordinationChange + "/json-v1"}
}

// CaptureCoordinationChange retains raw operands under a single bound identity.
// Callers establish absence and coverage before constructing a nil operand.
func CaptureCoordinationChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	operands := map[string]json.RawMessage{"before": json.RawMessage("null"), "after": json.RawMessage("null")}
	for _, entry := range []struct {
		key            string
		value          *protocol.Envelope
		representation protocol.ValueInput_Representation
	}{{"before", before, protocol.ValueInput_OBSERVED}, {"after", after, protocol.ValueInput_DESIRED}} {
		if entry.value == nil {
			continue
		}
		if !proto.Equal(owner, entry.value.Owner) {
			return nil, fmt.Errorf("coordination change operands have different owners")
		}
		if _, err := CoordinationConfiguration(entry.value, entry.representation); err != nil {
			return nil, err
		}
		operands[entry.key] = entry.value.Payload
	}
	payload, err := json.Marshal(operands)
	if err != nil {
		return nil, err
	}
	model := CoordinationChangeModel()
	return NormalizeCoordinationChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: model.Kind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: payload}})
}

// NormalizeCoordinationChange reconstructs safety facts from complete operands.
// A caller cannot supply grants, removal flags, or impact metadata in the payload.
func NormalizeCoordinationChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, contract := input.GetValue(), CoordinationChangeModel()
	if input.GetRepresentation() != protocol.ValueInput_CHANGE || value.GetProviderId() != ProviderID || value.GetKind() != contract.Kind || value.GetSchemaVersion() != contract.SchemaVersion || value.GetEncoding() != contract.Encoding || value.GetDefinitionSha256() != contract.DefinitionSha256 {
		return nil, fmt.Errorf("coordination change does not match its negotiated definition")
	}
	if err := desiredCoordinationOwner(value.GetOwner()); err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.Payload)
	if err != nil {
		return nil, err
	}
	var operands map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &operands); err != nil || len(operands) != 2 || operands["before"] == nil || operands["after"] == nil {
		return nil, fmt.Errorf("coordination change requires exact before and after operands")
	}
	present := map[string]bool{}
	for _, key := range []string{"before", "after"} {
		raw := operands[key]
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		present[key] = true
		definition, representation := CoordinationDesiredModel(), protocol.ValueInput_DESIRED
		if key == "before" {
			definition, representation = model(CoordinationProperties), protocol.ValueInput_OBSERVED
		}
		operand := &protocol.Envelope{ProviderId: ProviderID, Kind: CoordinationProperties, Owner: proto.CloneOf(value.Owner), SchemaVersion: 1, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: raw}
		if _, err := CoordinationConfiguration(operand, representation); err != nil {
			return nil, err
		}
		var normalized *protocol.Envelope
		if key == "before" {
			normalized, err = Normalize(operand)
		} else {
			normalized, err = NormalizeDesired(operand)
		}
		if err != nil {
			return nil, err
		}
		operands[key] = normalized.Payload
	}
	if !present["before"] && !present["after"] {
		return nil, fmt.Errorf("coordination change has no operands")
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
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: "Coordination settings change session behavior", Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Altering coordination settings preserves native access metadata"}
	if !present["before"] {
		semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_ADDITIVE, "Creates a coordination node", protocol.ChangeSemantics_ACCESS_UNKNOWN, "Creation inherits server access policy"
	}
	if !present["after"] {
		semantics.RemovesState, semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, "Drops the node and its runtime coordination state", protocol.ChangeSemantics_NARROWS, "Dropping the node removes access to it"
	}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: contract.CanonicalFormat, CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}
