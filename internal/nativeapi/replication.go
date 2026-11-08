package nativeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_Replication_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_Replication"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ErrReplicationServiceUnavailable is the answer of a cluster that does not
// serve YDB's replication API, which describes async replications and
// transfers. Measured on local-ydb 25.1.4.7 and 26.2.1.14 with their default
// configuration: a describe of a replication answers UNIMPLEMENTED, and
// describes it once the container starts with YDB_GRPC_SERVICES=replication.
var ErrReplicationServiceUnavailable = errors.New("the YDB cluster does not serve the replication API (its gRPC services lack replication)")

// Replications reads async replications and transfers through the
// replication service and changes them through YQL, as the host's embedded
// engine changes them.
type Replications struct {
	Scheme  Ydb_Scheme_V1.SchemeServiceClient
	Service Ydb_Replication_V1.ReplicationServiceClient
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
	// ReplicaWait is how long a creation that awaits its replica tables waits
	// for them, ReplicaWaitLimit when zero.
	ReplicaWait time.Duration
	// ReplicaPoll is how often it looks, replicaPollInterval when zero.
	ReplicaPoll time.Duration
}

// ReplicaWaitLimit is how long a replication creation that awaits its replica
// tables waits for them. With the source table in place, YDB created them
// within seconds of the statement on 25.1.4.7 and 26.2.1.14.
const ReplicaWaitLimit = 2 * time.Minute

const replicaPollInterval = 500 * time.Millisecond

// ReplicasNotCreated is the error of a replication creation that awaited its
// replica tables and found one still missing when the wait ran out. The
// replication stands; a unit after it reads the table, so the run stops.
type ReplicasNotCreated struct {
	// Replication and Table are the replication's path and the missing
	// table's, relative to the database root.
	Replication, Table string
	// Waited is how long the creation waited.
	Waited time.Duration
}

func (e *ReplicasNotCreated) Error() string {
	return fmt.Sprintf("async replication %s was created, but YDB had not created its replica table %s after %s; "+
		"the plan reads that table next, so it stops here; run the plan again once the table exists", e.Replication, e.Table, e.Waited)
}

// NewReplications binds the session's native clients and statement runner.
// A reader that only describes takes a nil execute.
func NewReplications(driver *ydb.Driver, execute func(context.Context, string) error) *Replications {
	conn := ydb.GRPCConn(driver)
	return &Replications{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), Service: Ydb_Replication_V1.NewReplicationServiceClient(conn), Execute: execute}
}

// DescribeReplication describes the async replication at absolute. An error
// wraps [ErrReplicationServiceUnavailable] where the server answered
// UNIMPLEMENTED.
func (r *Replications) DescribeReplication(ctx context.Context, absolute string) (*Ydb_Replication.DescribeReplicationResult, error) {
	response, err := r.Service.DescribeReplication(ctx, &Ydb_Replication.DescribeReplicationRequest{Path: absolute})
	if err != nil {
		return nil, replicationServiceError("describe YDB async replication", err)
	}
	var described Ydb_Replication.DescribeReplicationResult
	if err := unpack(response.GetOperation(), &described); err != nil {
		return nil, err
	}
	if described.GetSelf().GetName() != path.Base(absolute) || described.GetSelf().GetType() != Ydb_Scheme.Entry_REPLICATION {
		return nil, fmt.Errorf("YDB listing and async replication description disagree")
	}
	return &described, nil
}

// DescribeTransfer describes the transfer at absolute, as
// [Replications.DescribeReplication] describes a replication.
func (r *Replications) DescribeTransfer(ctx context.Context, absolute string) (*Ydb_Replication.DescribeTransferResult, error) {
	response, err := r.Service.DescribeTransfer(ctx, &Ydb_Replication.DescribeTransferRequest{Path: absolute})
	if err != nil {
		return nil, replicationServiceError("describe YDB transfer", err)
	}
	var described Ydb_Replication.DescribeTransferResult
	if err := unpack(response.GetOperation(), &described); err != nil {
		return nil, err
	}
	if described.GetSelf().GetName() != path.Base(absolute) || described.GetSelf().GetType() != Ydb_Scheme.Entry_TRANSFER {
		return nil, fmt.Errorf("YDB listing and transfer description disagree")
	}
	return &described, nil
}

// Capture describes the object of kind, an async replication or a transfer,
// at owner under root, the database root the session reads, and captures it
// as its observed model. A description the provider cannot read is an error
// wrapping [ErrUnreadable].
func (r *Replications) Capture(ctx context.Context, root string, owner *protocol.Reference) (*protocol.Envelope, error) {
	absolute := path.Join(owner.GetDatabase(), owner.GetSchema(), owner.GetName())
	var observed proto.Message
	switch owner.GetKind() {
	case schema.ReplicationProperties:
		described, err := r.DescribeReplication(ctx, absolute)
		if err != nil {
			return nil, err
		}
		spec, state, err := replication.DecodeReplication(root, described)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
		}
		observed = replication.ObservedReplication(spec, state)
	case schema.TransferProperties:
		described, err := r.DescribeTransfer(ctx, absolute)
		if err != nil {
			return nil, err
		}
		spec, state, err := replication.DecodeTransfer(root, described)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
		}
		observed = replication.ObservedTransfer(spec, state)
	default:
		return nil, fmt.Errorf("unsupported YDB replication object kind")
	}
	value, err := schema.CaptureObject(owner, observed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return value, nil
}

// ErrUnreadable marks a description the provider read and cannot model.
var ErrUnreadable = errors.New("the provider cannot read the description")

// unpack decodes a completed operation's result into target, keeping the
// fields the pinned protocol does not know for the caller to refuse.
func unpack(operation *Ydb_Operations.Operation, target proto.Message) error {
	if err := Status(operation); err != nil {
		return err
	}
	if err := operation.GetResult().UnmarshalTo(target); err != nil {
		return fmt.Errorf("invalid YDB operation response")
	}
	return nil
}

func replicationServiceError(subject string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%s: %w", subject, ErrReplicationServiceUnavailable)
	}
	return fmt.Errorf("%s: transport failed", subject)
}

// ApplyReplication verifies that the async replication is as the plan read
// it, or absent for a creation, runs the operation's statement, and reads
// the result back. Dispatched is true as soon as the statement was sent. A
// failed statement or an unconfirmed read leaves the outcome uncertain;
// nothing is retried. YDB has no atomic schema compare-and-set, so another
// writer can change the replication between the check and the statement.
//
// The statement names every path of this database absolutely, under the
// session's database root: the replication, each replica target and a
// secret named by its path. A source path belongs to the source database and
// is written as declared.
//
// An operation of the way [schema.ReplicationAwaitReplicas] then waits until
// each replica table it creates in this database exists, at most ReplicaWait.
// A table still missing then is a [ReplicasNotCreated] error, after the
// statement was dispatched.
func (r *Replications) ApplyReplication(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	name, err := schema.ReplicationOperationName(value)
	if err != nil {
		return false, err
	}
	dispatched, err := r.applyReplication(ctx, database, value, beforeWrite)
	if err != nil || name != schema.ReplicationAwaitReplicas {
		return dispatched, err
	}
	return true, r.awaitReplicas(ctx, database, value)
}

func (r *Replications) applyReplication(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	return applyStep(ctx, r, database, value, beforeWrite, stepOperation[schema.ReplicationStep]{
		noun:      "async replication",
		normalize: schema.NormalizeReplicationOperation,
		change:    schema.ReplicationOperationChange,
		operands:  schema.ReplicationOperands,
		stepOf:    schema.ReplicationStepOf,
		refuse: func(absolute string, step schema.ReplicationStep) *replication.Refusal {
			return replication.RefuseReplication(absolute, step.Before, step.After, step.State, replicationCapabilities)
		},
		statement: replicationStatement,
		confirm:   confirmReplication,
	})
}

// awaitReplicas waits until each replica table the replication creation value
// creates in the database at root exists, looking every ReplicaPoll, at most
// ReplicaWait.
func (r *Replications) awaitReplicas(ctx context.Context, root string, value *protocol.Envelope) error {
	normalized, err := schema.NormalizeReplicationOperation(&protocol.ValueInput{Id: "await", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return err
	}
	step, err := schema.ReplicationStepOf(schema.ReplicationOperationChange(normalized.Value))
	if err != nil {
		return err
	}
	if step.After == nil {
		return fmt.Errorf("async replication operation awaits replica tables of no creation")
	}
	owner := normalized.NormalizedOwner
	wait, poll := r.ReplicaWait, r.ReplicaPoll
	if wait <= 0 {
		wait = ReplicaWaitLimit
	}
	if poll <= 0 {
		poll = replicaPollInterval
	}
	deadline := time.Now().Add(wait)
	for _, target := range replication.Targets(*step.After) {
		absolute := path.Join(root, target)
		for {
			exists, err := r.tableExists(ctx, absolute)
			if err != nil {
				return err
			}
			if exists {
				break
			}
			if !time.Now().Before(deadline) {
				return &ReplicasNotCreated{Replication: path.Join(owner.GetSchema(), owner.GetName()), Table: target, Waited: wait}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
	}
	return nil
}

// tableExists reports whether YDB describes a table at absolute.
func (r *Replications) tableExists(ctx context.Context, absolute string) (bool, error) {
	response, err := r.Scheme.DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: absolute})
	if err != nil {
		return false, fmt.Errorf("describe replica table %s: transport failed", absolute)
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		return false, nil
	}
	var described Ydb_Scheme.DescribePathResult
	if err := unpack(response.GetOperation(), &described); err != nil {
		return false, err
	}
	return described.GetSelf().GetType() == Ydb_Scheme.Entry_TABLE, nil
}

// ApplyTransfer verifies, runs and confirms one transfer operation, as
// [Replications.ApplyReplication] does a replication's, except that the
// topic keeps its spelling (see [replication.AbsoluteTransfer]). A dev realm
// refuses a transfer before this runs, so the root is the database's.
func (r *Replications) ApplyTransfer(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	return applyStep(ctx, r, database, value, beforeWrite, stepOperation[schema.TransferStep]{
		noun:      "transfer",
		normalize: schema.NormalizeTransferOperation,
		change:    schema.TransferOperationChange,
		operands:  schema.TransferOperands,
		stepOf:    schema.TransferStepOf,
		refuse: func(absolute string, step schema.TransferStep) *replication.Refusal {
			return replication.RefuseTransfer(absolute, step.Before, step.After, step.State, transferCapabilities)
		},
		statement: transferStatement,
		confirm:   confirmTransfer,
	})
}

// stepOperation is what applying an async replication or a transfer
// operation takes from its kind: the noun its errors name, how the operation
// is normalized and read as a step, the refusal of a step YDB rejects on
// every line, the step's statement, and the confirmation of the result read
// back.
type stepOperation[Step any] struct {
	noun      string
	normalize func(*protocol.ValueInput) (*protocol.NormalizedValue, error)
	change    func(*protocol.Envelope) *protocol.Envelope
	operands  func(*protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error)
	stepOf    func(*protocol.Envelope) (Step, error)
	refuse    func(absolute string, step Step) *replication.Refusal
	statement func(root, absolute string, step Step) string
	confirm   func(step Step, current *protocol.Envelope) error
}

// applyStep verifies, runs and confirms one operation of the kind operation
// describes, as [Replications.ApplyReplication] says.
func applyStep[Step any](ctx context.Context, r *Replications, database string, value *protocol.Envelope, beforeWrite func(context.Context) error,
	operation stepOperation[Step],
) (bool, error) {
	normalized, err := operation.normalize(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner, err := boundOwner(normalized.NormalizedOwner, database, operation.noun)
	if err != nil {
		return false, err
	}
	change := operation.change(normalized.Value)
	before, _, err := operation.operands(change)
	if err != nil {
		return false, err
	}
	step, err := operation.stepOf(change)
	if err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	if refusal := operation.refuse(absolute, step); refusal != nil && refusal.Key == "" {
		return false, refusal
	}
	statement := operation.statement(database, absolute, step)
	if statement == "" {
		return false, fmt.Errorf("%s operation holds no statement", operation.noun)
	}
	return r.apply(ctx, database, owner, before, statement, beforeWrite, func(current *protocol.Envelope) error {
		return operation.confirm(step, current)
	})
}

// The capabilities execution checks a statement's own rules with: the
// planner checked the target's capabilities, and execution holds only the
// rules YDB applies on every line.
var (
	replicationCapabilities = map[string]bool{replication.CapabilityReplication: true, replication.CapabilitySecretPaths: true}
	transferCapabilities    = map[string]bool{replication.CapabilityTransfers: true, replication.CapabilitySecretPaths: true}
)

func boundOwner(normalized *protocol.Reference, database, noun string) (*protocol.Reference, error) {
	owner := proto.CloneOf(normalized)
	if owner.Database != "" && owner.Database != database {
		return nil, fmt.Errorf("%s operation targets another database", noun)
	}
	owner.Database = database
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return nil, err
	}
	return owner, nil
}

func (r *Replications) apply(ctx context.Context, database string, owner *protocol.Reference, before *protocol.Envelope, statement string,
	beforeWrite func(context.Context) error, confirm func(*protocol.Envelope) error,
) (bool, error) {
	current, err := r.Read(ctx, database, owner)
	if err != nil {
		return false, err
	}
	if (before == nil) != (current == nil) || (before != nil && !bytes.Equal(before.Payload, current.Payload)) {
		return false, fmt.Errorf("%s changed since the plan read it", path.Join(owner.Schema, owner.Name))
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := r.Execute(ctx, statement); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	observed, err := r.Read(ctx, database, owner)
	if err != nil {
		return true, err
	}
	return true, confirm(observed)
}

// Read lists the object's directory and captures the async replication or
// transfer owner names under root, or returns nil where the directory holds
// no entry of its name.
func (r *Replications) Read(ctx context.Context, root string, owner *protocol.Reference) (*protocol.Envelope, error) {
	directory := path.Join(owner.Database, owner.Schema)
	response, err := r.Scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return nil, fmt.Errorf("read YDB replication namespace: transport failed")
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		// A directory that is not there holds no object.
		return nil, nil
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := Operation(response.GetOperation(), &listing); err != nil {
		return nil, err
	}
	expected := Ydb_Scheme.Entry_REPLICATION
	if owner.GetKind() == schema.TransferProperties {
		expected = Ydb_Scheme.Entry_TRANSFER
	}
	found := false
	for _, entry := range listing.Children {
		if entry.GetName() != owner.Name {
			continue
		}
		if found || entry.GetType() != expected {
			return nil, fmt.Errorf("%s path has a different or ambiguous occupant", path.Join(owner.Schema, owner.Name))
		}
		found = true
	}
	if !found {
		return nil, nil
	}
	return r.Capture(ctx, root, owner)
}

// replicationStatement writes the statement of one replication change under
// root, or "" for a change that holds none.
func replicationStatement(root, absolute string, step schema.ReplicationStep) string {
	switch {
	case step.Before == nil:
		return replication.CreateReplicationStatement(absolute, replication.AbsoluteReplication(root, *step.After))
	case step.After == nil:
		return replication.DropReplicationStatement(absolute, step.Cascade())
	default:
		return replication.AlterReplicationStatement(absolute, replication.AbsoluteReplication(root, *step.After), replication.AbsoluteReplication(root, *step.Before))
	}
}

// transferStatement writes the statement of one transfer change under root,
// or "" for a change that holds none.
func transferStatement(root, absolute string, step schema.TransferStep) string {
	switch {
	case step.Before == nil:
		return replication.CreateTransferStatement(absolute, replication.AbsoluteTransfer(root, *step.After))
	case step.After == nil:
		return replication.DropTransferStatement(absolute)
	default:
		return replication.AlterTransferStatement(absolute, replication.AbsoluteTransfer(root, *step.After), replication.AbsoluteTransfer(root, *step.Before))
	}
}

// confirmReplication requires the replication read back to be the one the
// operation asked for: absent after a drop, and otherwise holding the
// declared settings.
func confirmReplication(step schema.ReplicationStep, current *protocol.Envelope) error {
	if step.After == nil {
		if current != nil {
			return fmt.Errorf("async replication removal was not confirmed")
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("async replication result is absent")
	}
	spec, _, err := schema.ObservedReplication(current)
	if err != nil {
		return err
	}
	if !replication.ReplicationsEqual(*step.After, spec) {
		return fmt.Errorf("async replication result differs from the requested settings")
	}
	return nil
}

// confirmTransfer requires the transfer read back to be the one the
// operation asked for.
func confirmTransfer(step schema.TransferStep, current *protocol.Envelope) error {
	if step.After == nil {
		if current != nil {
			return fmt.Errorf("transfer removal was not confirmed")
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("transfer result is absent")
	}
	spec, _, err := schema.ObservedTransfer(current)
	if err != nil {
		return err
	}
	if !replication.TransfersEqual(*step.After, spec) {
		return fmt.Errorf("transfer result differs from the requested settings")
	}
	return nil
}
