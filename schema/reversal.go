package schema

import (
	"bytes"
	"encoding/json"
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Reversed is the change that undoes one change of a standalone object, and
// what the forward change leaves.
type Reversed struct {
	// Value is the reverse change: its before operand is Left and its after
	// operand is the declaration of the object the forward change found.
	Value *protocol.NormalizedValue
	// Kind is the object model of the operands.
	Kind string
	// Before and After are the forward change's operands, either nil: what
	// the change found and what it asked for.
	Before, After *protocol.Envelope
	// Left is what the forward change leaves, as a read would report it, or
	// nil where it drops the object. It is a prediction from After, never
	// evidence that the forward change ran.
	Left *protocol.Envelope
	// Restored is the declaration the reverse asks for, the conversion of
	// Before, or nil where the reverse drops the object.
	Restored *protocol.Envelope
}

// reversibleObjects are the standalone object changes ReverseObjectChange
// undoes, by change kind.
var reversibleObjects = map[string]*objectChanges{
	SecretChange: secretChanges, TopicChange: topicChanges, StreamingQueryChange: streamingChanges,
	ExternalSourceChange: externalSourceChanges, ExternalTableChange: externalTableChanges,
	ResourcePoolChange: poolChanges, ResourcePoolClassifierChange: classifierChanges,
	ReplicationChange: replicationChanges, TransferChange: transferChanges,
}

// ReversibleObjectChanges lists the change kinds ReverseObjectChange undoes.
func ReversibleObjectChanges() []string {
	return []string{CoordinationChange, ExternalSourceChange, ExternalTableChange, ReplicationChange, ResourcePoolChange, ResourcePoolClassifierChange,
		SecretChange, StreamingQueryChange, TopicChange, TransferChange}
}

// ReverseObjectChange returns the change that undoes change, a change of one
// of ReversibleObjectChanges, under id. The reverse finds what the forward
// change leaves, the conversion of its declaration, and asks for what the
// forward change found, the conversion of that read; an absent operand stays
// absent, so a creation reverses to a drop and a drop to a creation. With
// commonAccess the restored declaration keeps no access entries, which travel
// as common grants. Each operand keeps the database context it was read or
// declared in.
func ReverseObjectChange(id string, change *protocol.Envelope, commonAccess bool) (Reversed, error) {
	if change.GetKind() == CoordinationChange {
		before, after, err := CoordinationOperands(change)
		if err != nil {
			return Reversed{}, err
		}
		left, restored, err := reversedOperands(before, after, ConvertCoordination, commonAccess)
		if err != nil {
			return Reversed{}, err
		}
		value, err := CaptureCoordinationChange(id, change.Owner, left, restored)
		if err != nil {
			return Reversed{}, err
		}
		return Reversed{Value: value, Kind: CoordinationProperties, Before: before, After: after, Left: left, Restored: restored}, nil
	}
	family, found := reversibleObjects[change.GetKind()]
	if !found {
		return Reversed{}, fmt.Errorf("YDB change %s has no reversal", change.GetKind())
	}
	if _, err := family.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: change}); err != nil {
		return Reversed{}, err
	}
	before, after, database, err := family.parts(change)
	if err != nil {
		return Reversed{}, err
	}
	left, restored, err := reversedOperands(before, after, ConvertNative, commonAccess)
	if err != nil {
		return Reversed{}, err
	}
	if restored, err = keepStateReset(family, after, restored); err != nil {
		return Reversed{}, err
	}
	value, err := family.captureIn(id, change.Owner, database, left, restored)
	if err != nil {
		return Reversed{}, err
	}
	return Reversed{Value: value, Kind: family.kind, Before: before, After: after, Left: left, Restored: restored}, nil
}

// reversedOperands converts the forward change's operands into the reverse
// change's: what the change asked for, as a read would report it, and what
// it found, as a declaration.
func reversedOperands(before, after *protocol.Envelope, convert func(*protocol.Envelope, protocol.ValueInput_Representation, protocol.ValueInput_Representation) (*protocol.Envelope, error),
	commonAccess bool,
) (left, restored *protocol.Envelope, err error) {
	if after != nil {
		if left, err = convert(after, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED); err != nil {
			return nil, nil, err
		}
		left.Context = proto.CloneOf(after.Context)
	}
	if before != nil {
		if restored, err = convert(before, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED); err != nil {
			return nil, nil, err
		}
		if commonAccess {
			if restored, err = WithoutAccess(restored); err != nil {
				return nil, nil, err
			}
		}
		restored.Context = proto.CloneOf(before.Context)
	}
	return left, restored, nil
}

// keepStateReset carries a streaming query's permission to reset its state
// from the forward declaration to the restored one. A read keeps no such
// permission, but the reverse of a text change the operator permitted resets
// the state again, and the streaming change refuses a text change without it.
func keepStateReset(family *objectChanges, after, restored *protocol.Envelope) (*protocol.Envelope, error) {
	if family != streamingChanges || after == nil || restored == nil {
		return restored, nil
	}
	forward, kept := &native.StreamingQueryIntent{}, &native.StreamingQueryIntent{}
	if err := protojson.Unmarshal(after.GetPayload(), forward); err != nil {
		return nil, err
	}
	if !forward.GetAllowStateReset() {
		return restored, nil
	}
	if err := protojson.Unmarshal(restored.GetPayload(), kept); err != nil {
		return nil, err
	}
	kept.AllowStateReset = true
	return convertedEnvelope(restored, NativeDesiredModel(StreamingQueryProperties), kept, NormalizeDesired)
}

// ReversibleTableChanges lists the change kinds ReverseTableChange undoes.
func ReversibleTableChanges() []string { return []string{ColumnStorageChange, TablePropertiesChange} }

// ReverseTableChange returns the change that undoes change, a change of a
// table's own settings, under id: the row table's properties, its index set
// or its changefeed set, or a column table's storage. Both operands of such a
// change are present, and the reverse swaps them as ReverseObjectChange does:
// what the forward change leaves, as a read would report it, and what it
// found, as a declaration. A column's change has no reversal: the host
// projects a reverse's state onto the table and its indexes, not onto a
// column.
func ReverseTableChange(id string, change *protocol.Envelope, commonAccess bool) (Reversed, error) {
	var before, after *protocol.Envelope
	var err error
	switch change.GetKind() {
	case TablePropertiesChange:
		before, after, err = tablePropertiesOperands(change)
	case ColumnStorageChange:
		before, after, err = ColumnStorageOperands(change)
		if err == nil && (before == nil || after == nil) {
			err = fmt.Errorf("a change between row and column storage has no reversal")
		}
	default:
		err = fmt.Errorf("YDB change %s has no reversal", change.GetKind())
	}
	if err != nil {
		return Reversed{}, err
	}
	convert := ConvertNative
	if before.Kind == TableProperties {
		convert = ConvertTable
	}
	left, restored, err := reversedOperands(before, after, convert, commonAccess)
	if before.Kind == TableProperties {
		// A table's properties are the whole table's description, and the
		// declaration states only its settings: what the forward change
		// leaves is the description the table held with the declared
		// settings in place of its own.
		left, restored, err = reversedOperands(before, nil, convert, commonAccess)
		if err == nil {
			left, err = alteredTable(before, after)
		}
	}
	if err != nil {
		return Reversed{}, err
	}
	capture := CaptureTablePropertiesChange
	if change.GetKind() == ColumnStorageChange {
		capture = CaptureColumnStorageChange
	}
	value, err := capture(id, change.Owner, left, restored)
	if err != nil {
		return Reversed{}, err
	}
	return Reversed{Value: value, Kind: before.Kind, Before: before, After: after, Left: left, Restored: restored}, nil
}

// tablePropertiesOperands returns the normalized operands of a table
// properties change of a table: its properties, its index set or its
// changefeed set, each under the change's owner.
func tablePropertiesOperands(change *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	if change.GetOwner().GetKind() != "table" {
		return nil, nil, fmt.Errorf("a column's change of family has no reversal")
	}
	normalized, err := NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "operands", Representation: protocol.ValueInput_CHANGE, Value: change})
	if err != nil {
		return nil, nil, err
	}
	var operands map[string]json.RawMessage
	if err := json.Unmarshal(normalized.Value.Payload, &operands); err != nil {
		return nil, nil, err
	}
	kind := TableProperties
	if selector, found := operands[facetKey]; found {
		if err := json.Unmarshal(selector, &kind); err != nil {
			return nil, nil, err
		}
	}
	operand := func(key string, definition *protocol.ValueModel) *protocol.Envelope {
		return &protocol.Envelope{ProviderId: ProviderID, Kind: kind, Owner: proto.CloneOf(change.Owner), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding,
			DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(operands[key])}
	}
	return operand("before", model(kind)), operand("after", desiredModel(kind)), nil
}

// alteredTable is the description a table holding before reads back with
// once an ALTER gives it the settings after declares: the settings are the
// declaration's, and what a declaration does not state, the columns, the key
// and the indexes among them, stays as the table held it.
func alteredTable(before, after *protocol.Envelope) (*protocol.Envelope, error) {
	held, declared := &native.DescribeTableResult{}, &native.RowTableIntent{}
	if err := protojson.Unmarshal(before.GetPayload(), held); err != nil {
		return nil, err
	}
	normalized, err := normalizeTableIntent(after)
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(normalized.GetPayload(), declared); err != nil {
		return nil, err
	}
	settings := tableObservation(declared)
	result := proto.CloneOf(held)
	// A table the declaration gives no TTL loses the one it holds, as the
	// comparison reads it; a setting the declaration leaves unset otherwise
	// stays as the table holds it.
	result.TtlSettings, result.Tiering = settings.TtlSettings, settings.Tiering
	if settings.StorageSettings != nil {
		result.StorageSettings = settings.StorageSettings
	}
	if settings.PartitioningSettings != nil {
		result.PartitioningSettings = settings.PartitioningSettings
	}
	if settings.KeyBloomFilter != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		result.KeyBloomFilter = settings.KeyBloomFilter
	}
	if settings.ReadReplicasSettings != nil {
		result.ReadReplicasSettings = settings.ReadReplicasSettings
	}
	if len(settings.ColumnFamilies) != 0 {
		result.ColumnFamilies = settings.ColumnFamilies
	}
	if len(settings.Attributes) != 0 {
		result.Attributes = settings.Attributes
	}
	altered, err := convertedEnvelope(before, model(TableProperties), result, Normalize)
	if err != nil {
		return nil, err
	}
	altered.Context = proto.CloneOf(after.Context)
	return altered, nil
}
