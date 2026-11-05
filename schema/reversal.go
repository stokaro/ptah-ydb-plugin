package schema

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
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
