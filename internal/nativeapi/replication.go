package nativeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
func (r *Replications) ApplyReplication(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	normalized, err := schema.NormalizeReplicationOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner, err := boundOwner(normalized.NormalizedOwner, database, "async replication")
	if err != nil {
		return false, err
	}
	change := schema.ReplicationOperationChange(normalized.Value)
	before, _, err := schema.ReplicationOperands(change)
	if err != nil {
		return false, err
	}
	step, err := schema.ReplicationStepOf(change)
	if err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	if refusal := replication.RefuseReplication(absolute, step.Before, step.After, step.State, replicationCapabilities); refusal != nil && refusal.Key == "" {
		return false, refusal
	}
	statement := replicationStatement(database, absolute, step)
	if statement == "" {
		return false, fmt.Errorf("async replication operation holds no statement")
	}
	return r.apply(ctx, database, owner, before, statement, beforeWrite, func(current *protocol.Envelope) error {
		return confirmReplication(step, current)
	})
}

// ApplyTransfer verifies, runs and confirms one transfer operation, as
// [Replications.ApplyReplication] does a replication's, except that the
// topic keeps its spelling (see [replication.AbsoluteTransfer]). A dev realm
// refuses a transfer before this runs, so the root is the database's.
func (r *Replications) ApplyTransfer(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	normalized, err := schema.NormalizeTransferOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner, err := boundOwner(normalized.NormalizedOwner, database, "transfer")
	if err != nil {
		return false, err
	}
	change := schema.TransferOperationChange(normalized.Value)
	before, _, err := schema.TransferOperands(change)
	if err != nil {
		return false, err
	}
	step, err := schema.TransferStepOf(change)
	if err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	if refusal := replication.RefuseTransfer(absolute, step.Before, step.After, step.State, transferCapabilities); refusal != nil && refusal.Key == "" {
		return false, refusal
	}
	statement := transferStatement(database, absolute, step)
	if statement == "" {
		return false, fmt.Errorf("transfer operation holds no statement")
	}
	return r.apply(ctx, database, owner, before, statement, beforeWrite, func(current *protocol.Envelope) error {
		return confirmTransfer(step, current)
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
