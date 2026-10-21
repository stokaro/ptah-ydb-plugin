package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"google.golang.org/protobuf/proto"
)

// CoordinationOperation identifies an executable, complete configuration transition.
const CoordinationOperation = "ydb.coordination-node-operation"

// CoordinationOperationModel defines native operations and their preconditions.
func CoordinationOperationModel() *protocol.ValueModel {
	return proto.CloneOf(coordinationOperationModel)
}

var coordinationOperationModel = buildCoordinationOperationModel()

func buildCoordinationOperationModel() *protocol.ValueModel {
	data, err := json.Marshal(map[string]any{"kind": CoordinationOperation, "representation": "operation", "schema_version": 1,
		"payload":       json.RawMessage(CoordinationChangeModel().DefinitionJson),
		"execution":     "native CreateNode, AlterNode, or DropNode; no SQL text; no automatic mutation retry; outside SQL transactions; require an existing parent directory",
		"precondition":  "before is the complete normalized observation or established absence; verify configuration and access metadata immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation":  "read back effective configuration after create/alter or absence after drop; alter must preserve access metadata; a successful create does not establish exclusive ownership",
		"database":      "a nonempty owner database must equal the selected session database; empty selects that session database for offline creation",
		"configuration": "send all effective settings; never replace the node to alter settings; semantic no-op operations refused"})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(data)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: CoordinationOperation, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: CoordinationOperation + "/json-v1"}
}

// CoordinationOperands returns independently normalized before/after envelopes.
// This decoder accepts the change model; operations normalize through that model.
func CoordinationOperands(value *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	normalized, err := NormalizeCoordinationChange(&protocol.ValueInput{Id: "operands", Representation: protocol.ValueInput_CHANGE, Value: value})
	if err != nil {
		return nil, nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(normalized.Value.Payload, &payload); err != nil {
		return nil, nil, err
	}
	operand := func(key string, definition *protocol.ValueModel) *protocol.Envelope {
		if bytes.Equal(payload[key], []byte("null")) {
			return nil
		}
		return &protocol.Envelope{ProviderId: ProviderID, Kind: CoordinationProperties, Owner: proto.CloneOf(value.Owner), SchemaVersion: 1, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(payload[key])}
	}
	return operand("before", model(CoordinationProperties)), operand("after", CoordinationDesiredModel()), nil
}

// CoordinationOperationFromChange lowers a complete change without losing raw operands.
func CoordinationOperationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	if _, err := NormalizeCoordinationChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: change}); err != nil {
		return nil, err
	}
	value := proto.CloneOf(change)
	definition := CoordinationOperationModel()
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = definition.Kind, definition.Encoding, definition.SchemaVersion, definition.DefinitionSha256
	return NormalizeCoordinationOperation(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: value})
}

// NormalizeCoordinationOperation reconstructs operation facts and refuses no-ops.
func NormalizeCoordinationOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), CoordinationOperationModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != ProviderID || value.GetKind() != definition.Kind || value.GetSchemaVersion() != definition.SchemaVersion || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("coordination operation does not match its negotiated definition")
	}
	change := CoordinationOperationChange(value)
	captured, err := NormalizeCoordinationChange(&protocol.ValueInput{Id: input.Id, Representation: protocol.ValueInput_CHANGE, Value: change})
	if err != nil {
		return nil, err
	}
	before, after, err := CoordinationOperands(captured.Value)
	if err != nil {
		return nil, err
	}
	if before != nil && after != nil {
		old, err := CoordinationConfiguration(before, protocol.ValueInput_OBSERVED)
		if err != nil {
			return nil, err
		}
		wanted, err := CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
		if err != nil {
			return nil, err
		}
		a, err := EffectiveCoordination(old)
		if err != nil {
			return nil, err
		}
		b, err := EffectiveCoordination(wanted)
		if err != nil {
			return nil, err
		}
		if a == b {
			return nil, fmt.Errorf("coordination operation contains no configuration change")
		}
	}
	result := proto.CloneOf(value)
	result.Payload = bytes.Clone(captured.Value.Payload)
	facts := captured.ChangeSemantics
	return &protocol.NormalizedValue{Id: input.Id, Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(result.Payload), OperationSemantics: &protocol.OperationSemantics{Impact: facts.Impact, Reason: facts.Reason, Access: facts.Access, AccessReason: facts.AccessReason}}, nil
}

// CoordinationOperationChange restores the complete change envelope of an operation.
// Callers validate the operation contract before using the returned operands.
func CoordinationOperationChange(operation *protocol.Envelope) *protocol.Envelope {
	value := proto.CloneOf(operation)
	if value == nil {
		return nil
	}
	definition := CoordinationChangeModel()
	value.Kind, value.Encoding, value.SchemaVersion, value.DefinitionSha256 = definition.Kind, definition.Encoding, definition.SchemaVersion, definition.DefinitionSha256
	return value
}

// CoordinationNativeConfig sends effective settings explicitly on create and alter.
func CoordinationNativeConfig(intent CoordinationIntent) (*Ydb_Coordination.Config, error) {
	settings, err := EffectiveCoordination(intent)
	if err != nil {
		return nil, err
	}
	mode := func(value string) Ydb_Coordination.ConsistencyMode {
		if value == "strict" {
			return Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT
		}
		return Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED
	}
	counters := Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_AGGREGATED
	if settings.CountersMode == "detailed" {
		counters = Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED
	}
	return &Ydb_Coordination.Config{SelfCheckPeriodMillis: settings.SelfCheckMillis, SessionGracePeriodMillis: settings.SessionGraceMillis, ReadConsistencyMode: mode(settings.ReadMode), AttachConsistencyMode: mode(settings.AttachMode), RateLimiterCountersMode: counters}, nil
}

// CoordinationBoundOwner checks exact bound components without parsing names.
func CoordinationBoundOwner(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
	if err := provider.ValidateComparisonIdentity(subject); err != nil {
		return nil, err
	}
	if subject.Kind != CoordinationProperties || subject.GetCatalog().GetSource() != "" || subject.GetParent().GetSource() != "" || len(subject.Path)+len(subject.Signature) != 0 {
		return nil, fmt.Errorf("invalid coordination comparison identity")
	}
	for _, part := range []*protocol.ComparisonPart{subject.Database, subject.Catalog, subject.Schema, subject.Name} {
		if part.GetSource() != part.GetNormalized() {
			return nil, fmt.Errorf("YDB identifiers must retain exact spelling")
		}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: CoordinationProperties, Database: subject.GetDatabase().GetNormalized(), Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
	return owner, ValidateCoordinationOwner(owner)
}
