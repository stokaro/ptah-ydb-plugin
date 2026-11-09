package schema

import (
	"google.golang.org/protobuf/encoding/protojson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// reached is the reverse of a change as YDB reaches it in place: what the
// forward change leaves, the declaration the reverse asks for, and whether
// the reverse runs anything. One that runs nothing asks for no declaration.
type reached struct {
	left, restored *protocol.Envelope
	runs           bool
}

// rollbackReached returns the reverse of a change of family as YDB reaches it
// in place, from the forward change's operands before and after and the
// reverse operands left and restored that reversedOperands made of them, as
// the host's embedded engine reversed such a change.
//
// A topic keeps every partition the change gave it and keeps
// auto-partitioning, paused, once the change turned it on, since YDB removes
// neither. An async replication or a transfer runs in the state the change
// found it in, and keeps a credential the change added, since YDB has no
// statement that takes one away. A replication failed over before the change
// dropped it is not created again: its tables stayed as ordinary tables.
func rollbackReached(family *objectChanges, before, after, left, restored *protocol.Envelope) (reached, error) {
	switch {
	case before == nil:
		return reached{left: left, restored: restored, runs: true}, nil
	case family == topicChanges && after != nil:
		return rollbackTopic(after, left, restored)
	case family == replicationChanges:
		return rollbackReplication(before, after, left, restored)
	case family == transferChanges && after != nil:
		return rollbackTransfer(before, after, left, restored)
	default:
		return reached{left: left, restored: restored, runs: true}, nil
	}
}

// rollbackTopic is rollbackReached for a topic the change altered.
func rollbackTopic(after, left, restored *protocol.Envelope) (reached, error) {
	_, declared, err := topicOperandSpecs(nil, after)
	if err != nil {
		return reached{}, err
	}
	intent, err := DesiredTopic(restored)
	if err != nil {
		return reached{}, err
	}
	target, err := topics.FromIntent(intent)
	if err != nil {
		return reached{}, err
	}
	nearest := topicRollbackTarget(target, *declared)
	if topics.Equal(nearest, *declared) {
		return reached{left: left}, nil
	}
	partitioned, err := topics.Intent(nearest)
	if err != nil {
		return reached{}, err
	}
	intent.PartitioningSettings = partitioned.PartitioningSettings
	restored, err = convertedEnvelope(restored, NativeDesiredModel(TopicProperties), intent, NormalizeDesired)
	return reached{left: left, restored: restored, runs: true}, err
}

// topicRollbackTarget is the topic nearest target that YDB reaches in place
// from current: target with its partition count raised to current's, and with
// auto-partitioning paused rather than disabled where current has it on.
func topicRollbackTarget(target, current ydbdecl.Topic) ydbdecl.Topic {
	nearest := target
	want, have := topics.Resolve(target), topics.Resolve(current)
	if want.MinActivePartitions < have.MinActivePartitions {
		nearest.MinActivePartitions = have.MinActivePartitions
	}
	if have.AutoPartitioned() && !want.AutoPartitioned() {
		nearest.AutoPartitioningStrategy = ydbdecl.StrategyPaused
		nearest.MaxActivePartitions = have.MaxActivePartitions
		nearest.AutoPartitioningUpUtilizationPercent = have.UpUtilizationPercent
		nearest.AutoPartitioningDownUtilizationPercent = have.DownUtilizationPercent
		nearest.AutoPartitioningStabilizationWindow = topics.FormatSeconds(have.StabilizationWindowSeconds)
	}
	if nearest.MaxActivePartitions != 0 && nearest.MaxActivePartitions < nearest.MinActivePartitions {
		nearest.MaxActivePartitions = nearest.MinActivePartitions
	}
	return nearest
}

// rollbackReplication is rollbackReached for an async replication the change
// found.
func rollbackReplication(before, after, left, restored *protocol.Envelope) (reached, error) {
	held, state, err := ObservedReplication(before)
	if err != nil {
		return reached{}, err
	}
	if after == nil {
		if state == replication.StateDone {
			return reached{}, nil
		}
		return reached{left: left, restored: restored, runs: true}, nil
	}
	declared, err := DesiredReplication(after)
	if err != nil {
		return reached{}, err
	}
	predicted := &native.AsyncReplicationSettings{}
	if err := protojson.Unmarshal(left.GetPayload(), predicted); err != nil {
		return reached{}, err
	}
	predicted.State = state
	if left, err = convertedEnvelope(left, model(ReplicationProperties), predicted, Normalize); err != nil {
		return reached{}, err
	}
	target := held.Clone()
	target.Connection = replication.RollbackConnection(held.Connection, declared.Connection)
	if replication.ReplicationsEqual(target, declared) {
		return reached{left: left}, nil
	}
	if target.Connection != held.Connection {
		restored, err = convertedEnvelope(restored, NativeDesiredModel(ReplicationProperties), replication.DesiredReplication(target), NormalizeDesired)
	}
	return reached{left: left, restored: restored, runs: true}, err
}

// rollbackTransfer is rollbackReached for a transfer the change altered.
func rollbackTransfer(before, after, left, restored *protocol.Envelope) (reached, error) {
	held, state, err := ObservedTransfer(before)
	if err != nil {
		return reached{}, err
	}
	declared, err := DesiredTransfer(after)
	if err != nil {
		return reached{}, err
	}
	predicted := &native.TransferSettings{}
	if err := protojson.Unmarshal(left.GetPayload(), predicted); err != nil {
		return reached{}, err
	}
	predicted.State = state
	if left, err = convertedEnvelope(left, model(TransferProperties), predicted, Normalize); err != nil {
		return reached{}, err
	}
	target := held
	target.Connection = replication.RollbackConnection(held.Connection, declared.Connection)
	if replication.TransfersEqual(target, declared) {
		return reached{left: left}, nil
	}
	if target.Connection != held.Connection {
		restored, err = convertedEnvelope(restored, NativeDesiredModel(TransferProperties), replication.DesiredTransfer(target), NormalizeDesired)
	}
	return reached{left: left, restored: restored, runs: true}, err
}
