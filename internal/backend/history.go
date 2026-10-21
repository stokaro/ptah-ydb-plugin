package backend

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/history"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (p *Provider) historySession(ctx context.Context, scope *protocol.HistoryScope) (*session, history.Store, func(), error) {
	s, err := p.lookup(scope.GetSessionId())
	if err != nil {
		return nil, history.Store{}, nil, err
	}
	s.mu.Lock()
	fail := func(err error) (*session, history.Store, func(), error) {
		s.mu.Unlock()
		return nil, history.Store{}, nil, err
	}
	if s.closed {
		return fail(status.Error(codes.FailedPrecondition, "YDB session is closed"))
	}
	if err := ctx.Err(); err != nil {
		return fail(status.FromContextError(err).Err())
	}
	if scope.TransactionId == "" {
		if s.active != "" {
			return fail(status.Error(codes.FailedPrecondition, "YDB session has an active transaction"))
		}
	} else {
		record := s.transactions[scope.TransactionId]
		if record == nil || record.tx == nil || s.active != scope.TransactionId || record.failed {
			return fail(status.Error(codes.FailedPrecondition, "YDB transaction is not active for history"))
		}
	}
	store, err := history.Resolve(s.database, scope)
	if err != nil {
		return fail(status.Error(codes.InvalidArgument, err.Error()))
	}
	return s, store, s.mu.Unlock, nil
}

func historyResult(scope *protocol.HistoryScope, id string, outcome protocol.HistoryResult_Outcome, code string) *protocol.HistoryResult {
	return &protocol.HistoryResult{Scope: proto.CloneOf(scope), RequestId: id, Outcome: outcome, DiagnosticCode: code}
}

// PrepareHistory creates owned storage and verifies its format. It never adopts
// an unmarked table, changes revision rows, or upgrades an earlier layout.
func (p *Provider) PrepareHistory(ctx context.Context, request *protocol.PrepareHistoryRequest) (*protocol.HistoryResult, error) {
	if err := provider.ValidatePrepareHistoryRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.historySession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_REFUSED, "ydb.lock-guarantee-lost"), nil
	}
	defer stop()
	ctx = guarded
	attempted, err := store.Prepare(ctx, s.connection.Driver, s.verifyLocks)
	if s.verifyLocks(ctx) != nil && err == nil {
		err = errors.New("YDB lock guarantee lost")
	}
	if err != nil {
		outcome := protocol.HistoryResult_REFUSED
		if attempted {
			outcome = protocol.HistoryResult_UNKNOWN
		}
		return historyResult(request.Scope, request.RequestId, outcome, "ydb.history-preparation-unconfirmed"), nil
	}
	return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_APPLIED, ""), nil
}

// ReadHistory preserves the exact scope, complete rows, and missing-store state.
func (p *Provider) ReadHistory(ctx context.Context, request *protocol.ReadHistoryRequest) (result *protocol.HistorySnapshot, err error) {
	if err := provider.ValidateReadHistoryRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.historySession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer func() {
		if err != nil && request.Scope.TransactionId != "" {
			s.transactions[request.Scope.TransactionId].failed = true
		}
	}()
	exists, err := store.Check(ctx, s.connection.Driver)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	result = &protocol.HistorySnapshot{Scope: proto.CloneOf(request.Scope), Exists: exists, Complete: true}
	if !exists {
		return result, nil
	}
	var runner history.Runner
	if request.Scope.TransactionId != "" {
		runner = s.transactions[request.Scope.TransactionId].tx
	} else {
		conn, openErr := s.connection.DB.Conn(ctx)
		if openErr != nil {
			return nil, connectionError(ctx, "acquire YDB history connection")
		}
		defer func() {
			if closeErr := closeSQLConnection(conn); closeErr != nil {
				s.cleanupErr = errors.Join(s.cleanupErr, closeErr)
				result = nil
				err = status.Error(codes.Unavailable, "close YDB history connection failed")
			}
		}()
		runner = conn
	}
	result.Revisions, err = store.Read(ctx, runner, request.MaxRevisions)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if proto.Size(result) > history.MaxSnapshotBytes {
		return nil, status.Error(codes.ResourceExhausted, "YDB history exceeds snapshot size limit")
	}
	if err := provider.ValidateHistorySnapshot(request, result); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return result, nil
}

// MutateHistory performs an atomic full-row comparison on the caller transaction
// or one private serializable transaction. No failure authorizes a retry.
func (p *Provider) MutateHistory(ctx context.Context, request *protocol.MutateHistoryRequest) (*protocol.HistoryResult, error) {
	if err := provider.ValidateMutateHistoryRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, store, unlock, err := p.historySession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer unlock()
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_REFUSED, "ydb.lock-guarantee-lost"), nil
	}
	defer stop()
	ctx = guarded
	for _, record := range []*protocol.HistoryRevision{request.Expected, request.Replacement} {
		if record != nil {
			if _, err := store.Encode(record); err != nil {
				return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_REFUSED, "ydb.history-record-invalid"), nil
			}
		}
	}
	exists, err := store.Check(ctx, s.connection.Driver)
	if err != nil || !exists {
		return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_REFUSED, "ydb.history-storage-invalid"), nil
	}
	var outcome protocol.HistoryResult_Outcome
	if request.Scope.TransactionId != "" {
		record := s.transactions[request.Scope.TransactionId]
		outcome, err = store.Mutate(ctx, record.tx, request)
		if err != nil {
			record.failed = true
		}
	} else {
		outcome, err = s.mutatePrivateHistory(ctx, store, request)
	}
	if s.verifyLocks(ctx) != nil && err == nil {
		err = errors.New("YDB lock guarantee lost")
	}
	if err != nil {
		return historyResult(request.Scope, request.RequestId, protocol.HistoryResult_UNKNOWN, "ydb.history-mutation-unconfirmed"), nil
	}
	return historyResult(request.Scope, request.RequestId, outcome, ""), nil
}

func (s *session) mutatePrivateHistory(ctx context.Context, store history.Store, request *protocol.MutateHistoryRequest) (protocol.HistoryResult_Outcome, error) {
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return protocol.HistoryResult_REFUSED, err
	}
	defer func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			s.cleanupErr = errors.Join(s.cleanupErr, rollbackErr)
		}
	}()
	outcome, err := store.Mutate(ctx, tx, request)
	if err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	if outcome != protocol.HistoryResult_APPLIED {
		if err := tx.Rollback(); err != nil {
			return protocol.HistoryResult_UNKNOWN, err
		}
		return outcome, nil
	}
	if err := s.verifyLocks(ctx); err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	return protocol.HistoryResult_APPLIED, nil
}
