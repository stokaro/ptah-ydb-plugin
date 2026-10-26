package schema

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// StreamingQueryProperties identifies one streaming query.
	StreamingQueryProperties = modelNamespace + "streaming-query"
	// StreamingQueryChange identifies a complete before/after change of one
	// streaming query.
	StreamingQueryChange = modelNamespace + "streaming-query-change"
	// StreamingQueryOperation identifies the executable change of one
	// streaming query.
	StreamingQueryOperation = modelNamespace + "streaming-query-operation"
)

// streamingDefinition is what both models of a streaming query say beyond
// their message.
const streamingDefinition = "the body inside DO BEGIN ... END DO, holding data statements only; an unset run starts the query and an empty pool selects the pool default; " +
	"the declaration's allow_state_reset permits a change that resets the aggregation state and is not stored; runtime status, offsets and checkpoints are not modeled; access is not read"

var streamingChanges = newObjectChanges(objectChanges{kind: StreamingQueryProperties, change: StreamingQueryChange, operation: StreamingQueryOperation, noun: "streaming query",
	semantics: streamingSemantics,
	definition: map[string]any{
		"semantics":  "create: additive unless it may replace a query; drop, a replacing creation and a changed body: destructive, discard the checkpoint state; other alter: behavioral; access unchanged; never replaces owner",
		"comparison": "bodies compared as YQL tokens without whitespace and comments; run resolved to true and the pool to default where unset",
		"validation": "a pair that holds no change is refused, and so is a changed body the declaration does not permit to reset state",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE [OR REPLACE] STREAMING QUERY, ALTER STREAMING QUERY or DROP STREAMING QUERY, one query each, outside SQL transactions; a running query that changes is stopped first with its old body; no automatic retry",
		"precondition": "before is the query as .sys/streaming_queries reported it to the plan, or its established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the declared settings, or the absence after a drop",
		"database":     "a nonempty owner database must equal the selected session database; empty selects that session database",
	}})

// StreamingQueryChangeModel defines a streaming query change.
func StreamingQueryChangeModel() *protocol.ValueModel {
	return proto.CloneOf(streamingChanges.changeModel)
}

// StreamingQueryOperationModel defines a streaming query operation.
func StreamingQueryOperationModel() *protocol.ValueModel {
	return proto.CloneOf(streamingChanges.operationModel)
}

// CaptureStreamingQueryChange retains complete operands under one owner;
// either may be nil for an established absence.
func CaptureStreamingQueryChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return streamingChanges.capture(id, owner, before, after)
}

// NormalizeStreamingQueryChange reconstructs a change's facts from its
// operands.
func NormalizeStreamingQueryChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return streamingChanges.normalizeChange(input)
}

// StreamingQueryOperands returns a change's normalized operands, nil for an
// absent one.
func StreamingQueryOperands(change *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	return streamingChanges.operands(change)
}

// StreamingQueryOperationFromChange lowers a complete change to its
// operation.
func StreamingQueryOperationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return streamingChanges.operationFromChange(id, change)
}

// NormalizeStreamingQueryOperation reconstructs an operation's facts.
func NormalizeStreamingQueryOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return streamingChanges.normalizeOperation(input)
}

// StreamingQueryOperationChange restores the change an operation carries.
func StreamingQueryOperationChange(operation *protocol.Envelope) *protocol.Envelope {
	return streamingChanges.operationChange(operation)
}

// StreamingChangeOf reads a normalized change as the query's change.
func StreamingChangeOf(change *protocol.Envelope) (streaming.Change, error) {
	before, after, err := StreamingQueryOperands(change)
	if err != nil {
		return streaming.Change{}, err
	}
	return streamingOperands(change.GetOwner(), before, after)
}

func streamingOperands(owner *protocol.Reference, before, after *protocol.Envelope) (streaming.Change, error) {
	change := streaming.Change{Schema: owner.GetSchema(), Name: owner.GetName()}
	if before != nil {
		spec, err := ObservedStreamingQuery(before)
		if err != nil {
			return streaming.Change{}, err
		}
		change.Before = &spec
	}
	if after != nil {
		spec, allow, err := DesiredStreamingQuery(after)
		if err != nil {
			return streaming.Change{}, err
		}
		change.After, change.AllowStateReset = &spec, allow
	}
	return change, nil
}

// ObservedStreamingQuery reads a streaming query observation.
func ObservedStreamingQuery(value *protocol.Envelope) (streaming.Spec, error) {
	if value.GetKind() != StreamingQueryProperties {
		return streaming.Spec{}, fmt.Errorf("expected a YDB streaming query observation")
	}
	normalized, err := Normalize(value)
	if err != nil {
		return streaming.Spec{}, err
	}
	var settings native.StreamingQuerySettings
	if err := protojson.Unmarshal(normalized.Payload, &settings); err != nil {
		return streaming.Spec{}, err
	}
	return streaming.Spec{Text: settings.GetText(), Run: settings.Run, ResourcePool: settings.GetResourcePool()}, nil
}

// DesiredStreamingQuery reads a streaming query declaration and its
// permission to reset state.
func DesiredStreamingQuery(value *protocol.Envelope) (streaming.Spec, bool, error) {
	if value.GetKind() != StreamingQueryProperties {
		return streaming.Spec{}, false, fmt.Errorf("expected a YDB streaming query declaration")
	}
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return streaming.Spec{}, false, err
	}
	var intent native.StreamingQueryIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return streaming.Spec{}, false, err
	}
	return streaming.Spec{Text: intent.GetText(), Run: intent.Run, ResourcePool: intent.GetResourcePool()}, intent.GetAllowStateReset(), nil
}

// CaptureStreamingQueryIntent writes a declared query as its desired model
// under owner.
func CaptureStreamingQueryIntent(owner *protocol.Reference, spec streaming.Spec, allowStateReset bool) (*protocol.Envelope, error) {
	intent := &native.StreamingQueryIntent{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool, AllowStateReset: allowStateReset}
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: StreamingQueryProperties, Owner: proto.CloneOf(owner)}, NativeDesiredModel(StreamingQueryProperties), intent, NormalizeDesired)
}

func streamingSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	owner := after.GetOwner()
	if before != nil {
		owner = before.GetOwner()
	}
	change, err := streamingOperands(owner, before, after)
	if err != nil {
		return nil, err
	}
	return streamingImpact(change)
}

func streamingImpact(change streaming.Change) (*protocol.ChangeSemantics, error) {
	if _, err := streaming.Statements(change); err != nil {
		return nil, err
	}
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "A streaming query change grants and revokes no permission"}
	if change.Before != nil && change.After != nil && streaming.Equal(*change.Before, *change.After) {
		return nil, fmt.Errorf("streaming query operands contain no change")
	}
	switch {
	case streaming.LosesCheckpoint(change):
		semantics.RemovesState, semantics.Impact, semantics.Reason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, streaming.CheckpointLoss
	case change.Before == nil:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_ADDITIVE, streaming.Creation
	default:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, streaming.ExecutionChange
	}
	return semantics, nil
}
