package backend

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (p *Provider) metadataSession(ctx context.Context, scope *protocol.MetadataScope) (*session, metadata.Store, func(), error) {
	s, err := p.lookup(scope.SessionId)
	if err != nil {
		return nil, metadata.Store{}, nil, err
	}
	s.mu.Lock()
	fail := func(err error) (*session, metadata.Store, func(), error) {
		s.mu.Unlock()
		return nil, metadata.Store{}, nil, err
	}
	if s.closed {
		return fail(status.Error(codes.FailedPrecondition, "YDB session is closed"))
	}
	if err := ctx.Err(); err != nil {
		return fail(status.FromContextError(err).Err())
	}
	if scope.TransactionId != "" {
		record := s.transactions[scope.TransactionId]
		if record == nil || record.tx == nil || record.failed || s.active != scope.TransactionId {
			return fail(status.Error(codes.FailedPrecondition, "YDB transaction is not active for metadata"))
		}
	} else if scope.Kind == protocol.MetadataScope_TAGS && s.active != "" {
		return fail(status.Error(codes.FailedPrecondition, "YDB tags require the active transaction"))
	}
	store, err := metadata.Resolve(s.database, scope)
	if err != nil {
		return fail(status.Error(codes.InvalidArgument, err.Error()))
	}
	return s, store, s.mu.Unlock, nil
}
func metadataResult(scope *protocol.MetadataScope, id string, outcome protocol.MetadataResult_Outcome) *protocol.MetadataResult {
	return &protocol.MetadataResult{Scope: proto.CloneOf(scope), RequestId: id, Outcome: outcome}
}
func metadataFailure(s *session, scope *protocol.MetadataScope, err error) error {
	if err != nil && scope.TransactionId != "" {
		s.transactions[scope.TransactionId].failed = true
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return status.FromContextError(err).Err()
		}
		if status.Code(err) == codes.ResourceExhausted {
			return status.Error(codes.ResourceExhausted, "YDB metadata resource limit reached")
		}
		return status.Error(codes.FailedPrecondition, "YDB metadata storage or operation could not be verified")
	}
	return nil
}

// PrepareMetadata creates marked native storage outside a caller transaction.
// Inside a transaction it validates existing storage and performs no DDL.
func (p *Provider) PrepareMetadata(ctx context.Context, request *protocol.PrepareMetadataRequest) (*protocol.MetadataResult, error) {
	if err := provider.ValidatePrepareMetadataRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.metadataSession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
	}
	defer stop()
	ctx = guarded
	var attempted bool
	if request.Scope.TransactionId != "" {
		exists, checkErr := store.Check(ctx, s.connection.Driver)
		if checkErr != nil || !exists {
			return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
		}
	} else {
		attempted, err = store.Prepare(ctx, s.connection.Driver, s.verifyLocks)
	}
	if s.verifyLocks(ctx) != nil && err == nil {
		err = errors.New("YDB lock guarantee lost")
	}
	outcome := protocol.MetadataResult_APPLIED
	if err != nil {
		outcome = protocol.MetadataResult_REJECTED
		if attempted {
			outcome = protocol.MetadataResult_UNKNOWN
		}
	}
	return metadataResult(request.Scope, request.RequestId, outcome), nil
}

type metadataWork struct {
	runner metadata.Runner
	tx     *sql.Tx
	conn   *sql.Conn
}

func (s *session) openMetadataWork(ctx context.Context, scope *protocol.MetadataScope) (*metadataWork, error) {
	if scope.TransactionId != "" {
		return &metadataWork{runner: s.transactions[scope.TransactionId].tx}, nil
	}
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "acquire YDB metadata connection failed")
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn))
		return nil, status.Error(codes.Unavailable, "begin YDB metadata transaction failed")
	}
	return &metadataWork{runner: tx, tx: tx, conn: conn}, nil
}
func (w *metadataWork) close() error {
	if w.tx == nil {
		return nil
	}
	var cleanup error
	if err := w.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		cleanup = errors.New("rollback YDB metadata transaction failed")
	}
	if err := closeSQLConnection(w.conn); err != nil {
		cleanup = errors.Join(cleanup, errors.New("close YDB metadata connection failed"))
	}
	return cleanup
}

// MutateMetadata appends durable events or changes tags in the selected transaction.
// Each mutation is dispatched once; uncertain writes never authorize replay.
func (p *Provider) MutateMetadata(ctx context.Context, request *protocol.MutateMetadataRequest) (*protocol.MetadataResult, error) {
	if err := provider.ValidateMutateMetadataRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.metadataSession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
	}
	defer stop()
	ctx = guarded
	// Record validation precedes storage checks and SQL transaction acquisition.
	var row proto.Message
	if request.GetAppendLog() != nil {
		row = request.GetAppendLog()
	} else if request.GetPutTag() != nil {
		row = request.GetPutTag()
	}
	if row != nil {
		if _, err := store.Encode(row); err != nil {
			return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
		}
	}
	exists, err := store.Check(ctx, s.connection.Driver)
	if err != nil {
		return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
	}
	if !exists {
		if err := s.verifyLocks(ctx); err != nil {
			return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
		}
		outcome := protocol.MetadataResult_REJECTED
		if request.GetDeleteTag() != "" {
			outcome = protocol.MetadataResult_APPLIED
		}
		return metadataResult(request.Scope, request.RequestId, outcome), nil
	}
	work, err := s.openMetadataWork(ctx, request.Scope)
	if err != nil {
		return metadataResult(request.Scope, request.RequestId, protocol.MetadataResult_REJECTED), nil
	}
	defer func() { s.cleanupErr = errors.Join(s.cleanupErr, work.close()) }()
	outcome, deleted, err := store.Mutate(ctx, work.runner, request, s.verifyLocks)
	if err != nil {
		_ = metadataFailure(s, request.Scope, err)
	}
	if outcome == protocol.MetadataResult_APPLIED && err == nil {
		if err = s.verifyLocks(ctx); err == nil && work.tx != nil {
			err = work.tx.Commit()
		}
		if err != nil {
			outcome = protocol.MetadataResult_UNKNOWN
			_ = metadataFailure(s, request.Scope, err)
		}
	}
	if s.verifyLocks(ctx) != nil && (outcome == protocol.MetadataResult_APPLIED || outcome == protocol.MetadataResult_UNKNOWN) {
		outcome = protocol.MetadataResult_UNKNOWN
		_ = metadataFailure(s, request.Scope, errors.New("YDB lock guarantee lost"))
	}
	result := metadataResult(request.Scope, request.RequestId, outcome)
	result.Deleted = deleted && outcome == protocol.MetadataResult_APPLIED
	return result, nil
}

// LookupMetadataTag preserves absence and exact signed versions without creating a store.
func (p *Provider) LookupMetadataTag(ctx context.Context, request *protocol.LookupMetadataTagRequest) (result *protocol.MetadataTagResult, err error) {
	if err := provider.ValidateLookupMetadataTagRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.metadataSession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer func() { err = metadataFailure(s, request.Scope, err) }()
	result = &protocol.MetadataTagResult{Scope: proto.CloneOf(request.Scope), RequestId: request.RequestId}
	exists, err := store.Check(ctx, s.connection.Driver)
	if err != nil {
		return nil, err
	}
	if !exists {
		return result, nil
	}
	work, err := s.openMetadataWork(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanup := work.close(); cleanup != nil {
			s.cleanupErr = errors.Join(s.cleanupErr, cleanup)
			result = nil
			err = errors.Join(err, cleanup)
		}
	}()
	result.Record, err = store.Lookup(ctx, work.runner, request.Name)
	if err != nil {
		return nil, err
	}
	result.Found = result.Record != nil
	if err := provider.ValidateMetadataTagResult(request, result); err != nil {
		return nil, err
	}
	return result, nil
}
