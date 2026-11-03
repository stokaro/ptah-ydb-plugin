package replication

import (
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// ValidateObservedReplication holds a replication read from the database to
// what a read can report: text without NUL, a state the reader names, a
// consistency level YDB has, a commit interval only at the global level and
// in whole milliseconds, and items that each name a source and a target. A
// replication read with no item is one that failed before it resolved its
// tables, which the read keeps.
func ValidateObservedReplication(spec ReplicationSpec, state string) error {
	if err := replicationText(spec); err != nil {
		return err
	}
	if err := validState(state); err != nil {
		return err
	}
	switch {
	case spec.ConsistencyLevel != "" && spec.ConsistencyLevel != ConsistencyRow && spec.ConsistencyLevel != ConsistencyGlobal:
		return fmt.Errorf("an observed consistency level is %s or %s, not %q", ConsistencyRow, ConsistencyGlobal, spec.ConsistencyLevel)
	case spec.CommitInterval != "" && spec.ConsistencyLevel != ConsistencyGlobal:
		return fmt.Errorf("an observed commit interval belongs to the global consistency level")
	}
	if spec.CommitInterval != "" {
		if _, err := CommitIntervalMillis(spec.CommitInterval); err != nil {
			return fmt.Errorf("observed commit interval %q %w", spec.CommitInterval, err)
		}
	}
	if slices.ContainsFunc(spec.Items, func(item Item) bool { return item.Source == "" || item.Target == "" }) {
		return fmt.Errorf("an observed item names a source and a target")
	}
	return nil
}

// ValidateObservedTransfer holds a transfer read from the database to what a
// read can report: text without NUL, a state the reader names, a source and a
// target, and a flush interval in whole seconds.
func ValidateObservedTransfer(spec TransferSpec, state string) error {
	if err := transferText(spec); err != nil {
		return err
	}
	if err := validState(state); err != nil {
		return err
	}
	if spec.Source == "" || spec.Target == "" {
		return fmt.Errorf("an observed transfer names its topic and its table")
	}
	if spec.FlushInterval != "" {
		if _, err := FlushIntervalSeconds(spec.FlushInterval); err != nil {
			return fmt.Errorf("observed flush interval %q %w", spec.FlushInterval, err)
		}
	}
	return nil
}

// ObservedReplication writes a replication read from the database as its
// observed model.
func ObservedReplication(spec ReplicationSpec, state string) *native.AsyncReplicationSettings {
	return &native.AsyncReplicationSettings{Connection: connectionMessage(spec.Connection), Items: itemMessages(spec.Items),
		ConsistencyLevel: spec.ConsistencyLevel, CommitInterval: spec.CommitInterval, State: state}
}

// FromObservedReplication reads an observed model back as the replication
// and the state it holds.
func FromObservedReplication(value *native.AsyncReplicationSettings) (ReplicationSpec, string) {
	return ReplicationSpec{Connection: connectionOf(value.GetConnection()), Items: itemsOf(value.GetItems()),
		ConsistencyLevel: value.GetConsistencyLevel(), CommitInterval: value.GetCommitInterval()}, value.GetState()
}

// DesiredReplication writes a declared replication as its desired model.
func DesiredReplication(spec ReplicationSpec) *native.AsyncReplicationIntent {
	return &native.AsyncReplicationIntent{Connection: connectionMessage(spec.Connection), Items: itemMessages(spec.Items),
		ConsistencyLevel: spec.ConsistencyLevel, CommitInterval: spec.CommitInterval}
}

// FromDesiredReplication reads a desired model back as the replication it
// declares.
func FromDesiredReplication(value *native.AsyncReplicationIntent) ReplicationSpec {
	return ReplicationSpec{Connection: connectionOf(value.GetConnection()), Items: itemsOf(value.GetItems()),
		ConsistencyLevel: value.GetConsistencyLevel(), CommitInterval: value.GetCommitInterval()}
}

// PredictedReplication is the replication YDB reports for a declaration
// before its state is known: the consistency level as YDB names it, row or
// global.
func PredictedReplication(spec ReplicationSpec) ReplicationSpec {
	predicted := spec.Clone()
	if predicted.ConsistencyLevel != "" {
		predicted.ConsistencyLevel = consistencyLevel(predicted)
	}
	return predicted
}

// ObservedTransfer writes a transfer read from the database as its observed
// model.
func ObservedTransfer(spec TransferSpec, state string) *native.TransferSettings {
	return &native.TransferSettings{Connection: connectionMessage(spec.Connection), Source: spec.Source, Target: spec.Target,
		Lambda: spec.Lambda, Consumer: spec.Consumer, BatchSizeBytes: spec.BatchSizeBytes, FlushInterval: spec.FlushInterval, State: state}
}

// FromObservedTransfer reads an observed model back as the transfer and the
// state it holds.
func FromObservedTransfer(value *native.TransferSettings) (TransferSpec, string) {
	return TransferSpec{Connection: connectionOf(value.GetConnection()), Source: value.GetSource(), Target: value.GetTarget(),
		Lambda: value.GetLambda(), Consumer: value.GetConsumer(), BatchSizeBytes: value.GetBatchSizeBytes(), FlushInterval: value.GetFlushInterval()}, value.GetState()
}

// DesiredTransfer writes a declared transfer as its desired model.
func DesiredTransfer(spec TransferSpec) *native.TransferIntent {
	return &native.TransferIntent{Connection: connectionMessage(spec.Connection), Source: spec.Source, Target: spec.Target,
		Lambda: spec.Lambda, Consumer: spec.Consumer, BatchSizeBytes: spec.BatchSizeBytes, FlushInterval: spec.FlushInterval}
}

// FromDesiredTransfer reads a desired model back as the transfer it
// declares.
func FromDesiredTransfer(value *native.TransferIntent) TransferSpec {
	return TransferSpec{Connection: connectionOf(value.GetConnection()), Source: value.GetSource(), Target: value.GetTarget(),
		Lambda: value.GetLambda(), Consumer: value.GetConsumer(), BatchSizeBytes: value.GetBatchSizeBytes(), FlushInterval: value.GetFlushInterval()}
}

func connectionMessage(connection Connection) *native.ReplicationConnection {
	if connection == (Connection{}) {
		return nil
	}
	return &native.ReplicationConnection{ConnectionString: connection.ConnectionString, TokenSecretName: connection.TokenSecretName,
		TokenSecretPath: connection.TokenSecretPath, User: connection.User, PasswordSecretName: connection.PasswordSecretName,
		PasswordSecretPath: connection.PasswordSecretPath}
}

func connectionOf(value *native.ReplicationConnection) Connection {
	return Connection{ConnectionString: value.GetConnectionString(), TokenSecretName: value.GetTokenSecretName(),
		TokenSecretPath: value.GetTokenSecretPath(), User: value.GetUser(), PasswordSecretName: value.GetPasswordSecretName(),
		PasswordSecretPath: value.GetPasswordSecretPath()}
}

func itemMessages(items []Item) []*native.ReplicationItem {
	var result []*native.ReplicationItem
	for _, item := range items {
		result = append(result, &native.ReplicationItem{Source: item.Source, Target: item.Target})
	}
	return result
}

func itemsOf(values []*native.ReplicationItem) []Item {
	var result []Item
	for _, value := range values {
		result = append(result, Item{Source: value.GetSource(), Target: value.GetTarget()})
	}
	return result
}
