package schema

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// ReplicationProperties identifies one async replication.
	ReplicationProperties = modelNamespace + "async-replication"
	// ReplicationChange identifies a complete before/after change of one
	// async replication.
	ReplicationChange = modelNamespace + "async-replication-change"
	// ReplicationOperation identifies the executable change of one async
	// replication.
	ReplicationOperation = modelNamespace + "async-replication-operation"
	// TransferProperties identifies one transfer.
	TransferProperties = modelNamespace + "transfer"
	// TransferChange identifies a complete before/after change of one
	// transfer.
	TransferChange = modelNamespace + "transfer-change"
	// TransferOperation identifies the executable change of one transfer.
	TransferOperation = modelNamespace + "transfer-operation"
)

// The reasons a replication or a transfer change gives for its impact, in the
// words of the host's embedded engine. Measured on 25.1.4.7 and 26.2.1.14: a
// replication dropped with CASCADE takes its replica tables with it, and one
// dropped without CASCADE leaves them read-only for good unless it was failed
// over first.
const (
	createReplicationReason = "CREATE ASYNC REPLICATION adds a replication and its replica tables"
	dropReplicationReason   = "DROP ASYNC REPLICATION ... CASCADE drops the replica tables with the replication"
	keepReplicaTablesReason = "DROP ASYNC REPLICATION without CASCADE ends the replication and keeps its tables, " +
		"read-only for good unless it was failed over first"
	alterReplicationReason  = "ALTER ASYNC REPLICATION points the replication at another source or credential"
	createTransferReason    = "CREATE TRANSFER adds a transfer"
	dropTransferReason      = "DROP TRANSFER stops the transfer and drops the topic consumer YDB created for it, with its position in the topic"
	alterTransferReason     = "ALTER TRANSFER changes the rows the transfer writes from each message"
	reconnectTransferReason = "ALTER TRANSFER points the transfer at another source or credential"
)

// replicationDefinition is what both models of a replication or a transfer
// say beyond their message.
const replicationDefinition = "the connection string as YDB rebuilds it from endpoint, database and TLS; a credential by the secret that holds it, " +
	"a secret path relative to the database root and a secret name as written, never a password or a token; " +
	"a replication's items with sources relative to the source database where they lie under it and targets relative to this database's root, in the order the server reports them; " +
	"a consistency level and commit interval, and a transfer's batch size and flush interval, only where they differ from YDB's defaults; " +
	"a transfer's lambda as written inside the $__ydb_transfer_lambda statement YDB stores; the observed state is running, paused, done, error or unknown; " +
	"runtime statistics, item ids, source changefeeds and access are not modeled here"

var replicationChanges = newObjectChanges(objectChanges{kind: ReplicationProperties, change: ReplicationChange, operation: ReplicationOperation, noun: "async replication",
	semantics: replicationSemantics,
	definition: map[string]any{
		"semantics":  "create: additive, access unknown; drop: destructive with CASCADE, which drops the replica tables, unless the replication was failed over (state done), when the tables stay and the drop is behavioral; alter: behavioral, access unchanged; never replaces owner",
		"comparison": "the connection string in its canonical form, the credential by kind and secret, the consistency level row and a global commit interval ten seconds where none is named, and the items by the tables they replicate",
		"validation": "a pair that holds no change is refused",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE ASYNC REPLICATION, ALTER ASYNC REPLICATION ... SET or DROP ASYNC REPLICATION [CASCADE], one query each, outside SQL transactions; no automatic retry",
		"precondition": "before is the replication as the replication service described it to the plan, or its established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the declared settings, or the absence after a drop",
		"database":     "a nonempty owner database must equal the selected session database; empty selects that session database",
	}})

var transferChanges = newObjectChanges(objectChanges{kind: TransferProperties, change: TransferChange, operation: TransferOperation, noun: "transfer",
	semantics: transferSemantics,
	definition: map[string]any{
		"semantics":  "create: additive, access unknown; drop: destructive, drops the consumer YDB created for the transfer; alter of the lambda or the batch settings, or of the connection: behavioral, access unchanged; never replaces owner",
		"comparison": "the lambda as text, the batch size 8 MiB and the flush interval a minute where none is named, the connection string in its canonical form, the credential by kind and secret, and the consumer as any where the declaration names none",
		"validation": "a pair that holds no change is refused",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE TRANSFER, ALTER TRANSFER or DROP TRANSFER, one query each, outside SQL transactions; no automatic retry",
		"precondition": "before is the transfer as the replication service described it to the plan, or its established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the declared settings, or the absence after a drop",
		"database":     "a nonempty owner database must equal the selected session database; empty selects that session database",
	}})

// ReplicationChangeModel defines an async replication change.
func ReplicationChangeModel() *protocol.ValueModel {
	return proto.CloneOf(replicationChanges.changeModel)
}

// ReplicationOperationModel defines an async replication operation.
func ReplicationOperationModel() *protocol.ValueModel {
	return proto.CloneOf(replicationChanges.operationModel)
}

// TransferChangeModel defines a transfer change.
func TransferChangeModel() *protocol.ValueModel { return proto.CloneOf(transferChanges.changeModel) }

// TransferOperationModel defines a transfer operation.
func TransferOperationModel() *protocol.ValueModel {
	return proto.CloneOf(transferChanges.operationModel)
}

// CaptureReplicationChange retains complete operands under one owner; either
// may be nil for an established absence.
func CaptureReplicationChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return replicationChanges.capture(id, owner, before, after)
}

// NormalizeReplicationChange reconstructs a change's facts from its operands.
func NormalizeReplicationChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return replicationChanges.normalizeChange(input)
}

// ReplicationOperands returns a change's normalized operands, nil for an
// absent one.
func ReplicationOperands(change *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	return replicationChanges.operands(change)
}

// ReplicationOperationFromChange lowers a complete change to its operation.
func ReplicationOperationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return replicationChanges.operationFromChange(id, change)
}

// NormalizeReplicationOperation reconstructs an operation's facts.
func NormalizeReplicationOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return replicationChanges.normalizeOperation(input)
}

// ReplicationOperationChange restores the change an operation carries.
func ReplicationOperationChange(operation *protocol.Envelope) *protocol.Envelope {
	return replicationChanges.operationChange(operation)
}

// CaptureTransferChange retains complete operands under one owner; either may
// be nil for an established absence.
func CaptureTransferChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return transferChanges.capture(id, owner, before, after)
}

// NormalizeTransferChange reconstructs a change's facts from its operands.
func NormalizeTransferChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return transferChanges.normalizeChange(input)
}

// TransferOperands returns a change's normalized operands, nil for an absent
// one.
func TransferOperands(change *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	return transferChanges.operands(change)
}

// TransferOperationFromChange lowers a complete change to its operation.
func TransferOperationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return transferChanges.operationFromChange(id, change)
}

// NormalizeTransferOperation reconstructs an operation's facts.
func NormalizeTransferOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return transferChanges.normalizeOperation(input)
}

// TransferOperationChange restores the change an operation carries.
func TransferOperationChange(operation *protocol.Envelope) *protocol.Envelope {
	return transferChanges.operationChange(operation)
}

// ReplicationStep is one change of an async replication read as the host's
// specifications: Before and its State as the database held them, nil for an
// absent one, and After as declared, nil for a drop.
type ReplicationStep struct {
	Before, After *replication.ReplicationSpec
	State         string
}

// Cascade reports whether dropping the replication drops its replica tables
// too: every replication but one failed over, whose tables are ordinary and
// stay.
func (s ReplicationStep) Cascade() bool { return s.Before != nil && s.State != replication.StateDone }

// TransferStep is one change of a transfer, as [ReplicationStep] is one of a
// replication.
type TransferStep struct {
	Before, After *replication.TransferSpec
	State         string
}

// ReplicationStepOf reads a normalized change as the replication's change.
func ReplicationStepOf(change *protocol.Envelope) (ReplicationStep, error) {
	before, after, err := ReplicationOperands(change)
	if err != nil {
		return ReplicationStep{}, err
	}
	return replicationStep(before, after)
}

// TransferStepOf reads a normalized change as the transfer's change.
func TransferStepOf(change *protocol.Envelope) (TransferStep, error) {
	before, after, err := TransferOperands(change)
	if err != nil {
		return TransferStep{}, err
	}
	return transferStep(before, after)
}

func replicationStep(before, after *protocol.Envelope) (ReplicationStep, error) {
	var step ReplicationStep
	if before != nil {
		spec, state, err := ObservedReplication(before)
		if err != nil {
			return ReplicationStep{}, err
		}
		step.Before, step.State = &spec, state
	}
	if after != nil {
		spec, err := DesiredReplication(after)
		if err != nil {
			return ReplicationStep{}, err
		}
		step.After = &spec
	}
	return step, nil
}

func transferStep(before, after *protocol.Envelope) (TransferStep, error) {
	var step TransferStep
	if before != nil {
		spec, state, err := ObservedTransfer(before)
		if err != nil {
			return TransferStep{}, err
		}
		step.Before, step.State = &spec, state
	}
	if after != nil {
		spec, err := DesiredTransfer(after)
		if err != nil {
			return TransferStep{}, err
		}
		step.After = &spec
	}
	return step, nil
}

// ObservedReplication reads a replication observation and the state it
// reports.
func ObservedReplication(value *protocol.Envelope) (replication.ReplicationSpec, string, error) {
	if value.GetKind() != ReplicationProperties {
		return replication.ReplicationSpec{}, "", fmt.Errorf("expected a YDB async replication observation")
	}
	normalized, err := Normalize(value)
	if err != nil {
		return replication.ReplicationSpec{}, "", err
	}
	var settings native.AsyncReplicationSettings
	if err := protojson.Unmarshal(normalized.Payload, &settings); err != nil {
		return replication.ReplicationSpec{}, "", err
	}
	spec, state := replication.FromObservedReplication(&settings)
	return spec, state, nil
}

// DesiredReplication reads a replication declaration.
func DesiredReplication(value *protocol.Envelope) (replication.ReplicationSpec, error) {
	if value.GetKind() != ReplicationProperties {
		return replication.ReplicationSpec{}, fmt.Errorf("expected a YDB async replication declaration")
	}
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return replication.ReplicationSpec{}, err
	}
	var intent native.AsyncReplicationIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return replication.ReplicationSpec{}, err
	}
	return replication.FromDesiredReplication(&intent), nil
}

// ObservedTransfer reads a transfer observation and the state it reports.
func ObservedTransfer(value *protocol.Envelope) (replication.TransferSpec, string, error) {
	if value.GetKind() != TransferProperties {
		return replication.TransferSpec{}, "", fmt.Errorf("expected a YDB transfer observation")
	}
	normalized, err := Normalize(value)
	if err != nil {
		return replication.TransferSpec{}, "", err
	}
	var settings native.TransferSettings
	if err := protojson.Unmarshal(normalized.Payload, &settings); err != nil {
		return replication.TransferSpec{}, "", err
	}
	spec, state := replication.FromObservedTransfer(&settings)
	return spec, state, nil
}

// DesiredTransfer reads a transfer declaration.
func DesiredTransfer(value *protocol.Envelope) (replication.TransferSpec, error) {
	if value.GetKind() != TransferProperties {
		return replication.TransferSpec{}, fmt.Errorf("expected a YDB transfer declaration")
	}
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return replication.TransferSpec{}, err
	}
	var intent native.TransferIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return replication.TransferSpec{}, err
	}
	return replication.FromDesiredTransfer(&intent), nil
}

// CaptureReplicationIntent writes a declared replication as its desired model
// under owner.
func CaptureReplicationIntent(owner *protocol.Reference, spec replication.ReplicationSpec) (*protocol.Envelope, error) {
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: ReplicationProperties, Owner: proto.CloneOf(owner)},
		NativeDesiredModel(ReplicationProperties), replication.DesiredReplication(spec), NormalizeDesired)
}

// CaptureTransferIntent writes a declared transfer as its desired model under
// owner.
func CaptureTransferIntent(owner *protocol.Reference, spec replication.TransferSpec) (*protocol.Envelope, error) {
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: TransferProperties, Owner: proto.CloneOf(owner)},
		NativeDesiredModel(TransferProperties), replication.DesiredTransfer(spec), NormalizeDesired)
}

func replicationSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	step, err := replicationStep(before, after)
	if err != nil {
		return nil, err
	}
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "Altering an async replication preserves its access metadata"}
	switch {
	case step.Before == nil:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_ADDITIVE, createReplicationReason
		semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_ACCESS_UNKNOWN, "Creation inherits server access policy"
	case step.After == nil:
		semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_NARROWS, "Dropping the replication removes access to it"
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, keepReplicaTablesReason
		if step.Cascade() {
			semantics.RemovesState, semantics.Impact, semantics.Reason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, dropReplicationReason
		}
	case replication.ReplicationsEqual(*step.After, *step.Before):
		return nil, fmt.Errorf("async replication operands contain no change")
	default:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, alterReplicationReason
	}
	return semantics, nil
}

func transferSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	step, err := transferStep(before, after)
	if err != nil {
		return nil, err
	}
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "Altering a transfer preserves its access metadata"}
	switch {
	case step.Before == nil:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_ADDITIVE, createTransferReason
		semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_ACCESS_UNKNOWN, "Creation inherits server access policy"
	case step.After == nil:
		semantics.RemovesState, semantics.Impact, semantics.Reason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, dropTransferReason
		semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_NARROWS, "Dropping the transfer removes access to it"
	case replication.TransfersEqual(*step.After, *step.Before):
		return nil, fmt.Errorf("transfer operands contain no change")
	default:
		changes := replication.CompareTransfer(*step.After, *step.Before)
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, reconnectTransferReason
		if changes.Lambda || changes.Batch {
			semantics.Reason = alterTransferReason
		}
	}
	return semantics, nil
}

// normalizeReplicationObject holds a replication or transfer observation to
// what a read reports.
func normalizeReplicationObject(described proto.Message) error {
	switch value := described.(type) {
	case *native.AsyncReplicationSettings:
		spec, state := replication.FromObservedReplication(value)
		return replication.ValidateObservedReplication(spec, state)
	case *native.TransferSettings:
		spec, state := replication.FromObservedTransfer(value)
		return replication.ValidateObservedTransfer(spec, state)
	}
	return fmt.Errorf("unsupported native YDB object model")
}

// replicationIntent converts an observation to the declaration that keeps it.
func replicationIntent(value proto.Message) (proto.Message, bool) {
	switch observed := value.(type) {
	case *native.AsyncReplicationSettings:
		spec, _ := replication.FromObservedReplication(observed)
		return replication.DesiredReplication(spec), true
	case *native.TransferSettings:
		spec, _ := replication.FromObservedTransfer(observed)
		return replication.DesiredTransfer(spec), true
	}
	return nil, false
}

// replicationObservation validates a declaration and predicts the
// observation YDB reports for it, its state unknown.
func replicationObservation(value proto.Message) (proto.Message, bool, error) {
	switch intent := value.(type) {
	case *native.AsyncReplicationIntent:
		spec := replication.FromDesiredReplication(intent)
		if err := replication.ValidateReplication(spec); err != nil {
			return nil, true, err
		}
		return replication.ObservedReplication(replication.PredictedReplication(spec), ""), true, nil
	case *native.TransferIntent:
		spec := replication.FromDesiredTransfer(intent)
		if err := replication.ValidateTransfer(spec); err != nil {
			return nil, true, err
		}
		return replication.ObservedTransfer(spec, ""), true, nil
	}
	return nil, false, nil
}

// replicationDeclaration is what the desired models of a replication and a
// transfer say beyond their message.
const replicationDeclaration = "the settings CREATE ASYNC REPLICATION and CREATE TRANSFER take, held to the host's declaration rules: " +
	"a replication reads another database through its connection string and replicates at least one item, targets relative to the database root and no two at one place; " +
	"a commit interval belongs to the global consistency level and keeps whole milliseconds; a transfer names its topic, its table and an inline lambda without a closing semicolon; " +
	"a flush interval keeps whole seconds; at most one credential, named by its secret; the state of a replication is never declared"
