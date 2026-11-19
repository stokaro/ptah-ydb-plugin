package backend

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/repeatables"
	"github.com/stokaro/ptah-ydb-plugin/internal/seeds"
)

// repeatableAttempts bounds how many times a write of the record runs again
// after YDB aborts its transaction for a conflicting one.
const repeatableAttempts = 8

// repeatableSession locks the session scope names and resolves the record
// the scope selects. On success the caller unlocks the session's mutex. The
// SDK's server refuses every call of the service before it arrives here when
// the host did not negotiate the record.
func (p *Provider) repeatableSession(ctx context.Context, scope *protocol.RepeatableScope) (*session, repeatables.Store, error) {
	s, err := p.lookup(scope.GetSessionId())
	if err != nil {
		return nil, repeatables.Store{}, err
	}
	s.mu.Lock()
	fail := func(err error) (*session, repeatables.Store, error) {
		s.mu.Unlock()
		return nil, repeatables.Store{}, err
	}
	if s.closed {
		return fail(runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed"))
	}
	if err := ctx.Err(); err != nil {
		return fail(status.FromContextError(err).Err())
	}
	if scope.GetTransactionId() == "" {
		if s.active != "" {
			return fail(runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session has an active transaction"))
		}
	} else {
		record := s.transactions[scope.GetTransactionId()]
		if record == nil || record.tx == nil || s.active != scope.GetTransactionId() || record.failed {
			return fail(runtime.ErrorWithReason(codes.FailedPrecondition, "YDB transaction is not active for the repeatable migration record"))
		}
	}
	store, err := repeatables.Resolve(s.database, scope)
	if err != nil {
		return fail(authored.Status(codes.InvalidArgument, err))
	}
	return s, store, nil
}

func repeatableResult(scope *protocol.RepeatableScope, id string, outcome protocol.RepeatableResult_Outcome) *protocol.RepeatableResult {
	return &protocol.RepeatableResult{Scope: proto.CloneOf(scope), RequestId: id, Outcome: outcome}
}

// withSessionConnection runs read on a connection of its own, outside any
// transaction, and returns it to the pool.
func (s *session) withSessionConnection(ctx context.Context, read func(repeatables.Runner) error) (err error) {
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return connectionError(ctx, "acquire a YDB connection for the repeatable migration record")
	}
	defer func() {
		if closeErr := closeSQLConnection(conn); closeErr != nil {
			s.cleanupErr = errors.Join(s.cleanupErr, closeErr)
			err = runtime.ErrorWithReason(codes.Unavailable, "close a YDB connection for the repeatable migration record failed")
		}
	}()
	return read(conn)
}

// ReadRepeatables returns every row of the record without bodies, from one
// query, or that the record does not exist. It creates nothing.
func (p *Provider) ReadRepeatables(ctx context.Context, request *protocol.ReadRepeatablesRequest) (*protocol.RepeatableSnapshot, error) {
	if err := provider.ValidateReadRepeatablesRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, store, err := p.repeatableSession(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	exists, err := store.Locate(ctx, s.connection.Driver)
	if err != nil {
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	result := &protocol.RepeatableSnapshot{Scope: proto.CloneOf(request.GetScope()), Exists: exists, Complete: true}
	if !exists {
		return result, nil
	}
	if err := s.withSessionConnection(ctx, func(runner repeatables.Runner) error {
		var readErr error
		result.Rows, readErr = store.Read(ctx, runner, request.GetMaxRows())
		return readErr
	}); err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	if err := provider.ValidateRepeatableSnapshotReply(request, result); err != nil {
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	return result, nil
}

// PrepareRepeatables creates the record once, outside any transaction, and
// confirms the ownership and layout of an existing one. It never adopts an
// unmarked table, upgrades a layout or changes a row.
func (p *Provider) PrepareRepeatables(ctx context.Context, request *protocol.PrepareRepeatablesRequest) (*protocol.RepeatableResult, error) {
	if err := provider.ValidatePrepareRepeatablesRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, store, err := p.repeatableSession(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return repeatableResult(request.GetScope(), request.GetRequestId(), protocol.RepeatableResult_REFUSED), nil
	}
	defer stop()
	ctx = guarded
	attempted, err := store.Prepare(ctx, s.connection.Driver, s.verifyLocks)
	if s.verifyLocks(ctx) != nil && err == nil {
		err = errors.New("YDB lock guarantee lost")
	}
	if err != nil {
		outcome := protocol.RepeatableResult_REFUSED
		if attempted {
			outcome = protocol.RepeatableResult_UNKNOWN
		}
		return repeatableResult(request.GetScope(), request.GetRequestId(), outcome), nil
	}
	return repeatableResult(request.GetScope(), request.GetRequestId(), protocol.RepeatableResult_APPLIED), nil
}

// ReadRepeatableBody returns the bytes the record keeps for one identity,
// whether the record holds a row for it and whether the row keeps a body.
func (p *Provider) ReadRepeatableBody(ctx context.Context, request *protocol.ReadRepeatableBodyRequest) (*protocol.RepeatableBody, error) {
	if err := provider.ValidateReadRepeatableBodyRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, store, err := p.repeatableSession(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	exists, err := store.Locate(ctx, s.connection.Driver)
	if err != nil {
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	result := &protocol.RepeatableBody{Scope: proto.CloneOf(request.GetScope()), Identity: request.GetIdentity()}
	if exists {
		if err := s.withSessionConnection(ctx, func(runner repeatables.Runner) error {
			stored, readErr := store.Body(ctx, runner, request.GetIdentity())
			result.Found, result.Kept, result.Body = stored.Found, stored.Kept, stored.Bytes
			return readErr
		}); err != nil {
			if _, ok := status.FromError(err); ok {
				return nil, err
			}
			return nil, authored.Status(codes.FailedPrecondition, err)
		}
	}
	if err := provider.ValidateRepeatableBodyReply(request, result); err != nil {
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	return result, nil
}

// MutateRepeatable writes one row atomically. A completion that carries a
// transaction reads and writes in it, and the caller's commit decides; every
// other write runs in a serializable transaction of its own, which runs
// again when YDB aborts it for a conflicting one, so of concurrent claims one
// applies and the others read the winner's row and meet a CONFLICT naming it.
// No failure permits a replay.
func (p *Provider) MutateRepeatable(ctx context.Context, request *protocol.MutateRepeatableRequest) (*protocol.RepeatableResult, error) {
	if err := provider.ValidateMutateRepeatableRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, store, err := p.repeatableSession(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	refused := repeatableResult(request.GetScope(), request.GetRequestId(), protocol.RepeatableResult_REFUSED)
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return refused, nil
	}
	defer stop()
	ctx = guarded
	exists, err := store.Locate(ctx, s.connection.Driver)
	if err != nil || !exists {
		return refused, nil
	}
	var written repeatables.Result
	if id := request.GetScope().GetTransactionId(); id != "" {
		record := s.transactions[id]
		written, err = store.Mutate(ctx, record.tx, request, s.verifyLocks)
		if err != nil {
			record.failed = true
		}
	} else {
		written, err = s.mutatePrivateRepeatable(ctx, store, request)
	}
	if s.verifyLocks(ctx) != nil && err == nil {
		err = errors.New("YDB lock guarantee lost")
	}
	if err != nil {
		return repeatableResult(request.GetScope(), request.GetRequestId(), protocol.RepeatableResult_UNKNOWN), nil
	}
	result := repeatableResult(request.GetScope(), request.GetRequestId(), written.Outcome)
	result.HolderRunId, result.Removed = written.Holder, written.Removed
	return result, nil
}

// mutatePrivateRepeatable runs the write in a serializable transaction of its
// own, and runs it again when YDB aborts it for a conflicting transaction or
// refuses an insert another transaction made first: either proves that
// nothing of it committed, and the next attempt reads the row the other
// transaction wrote.
func (s *session) mutatePrivateRepeatable(ctx context.Context, store repeatables.Store, request *protocol.MutateRepeatableRequest) (repeatables.Result, error) {
	for attempt := range repeatableAttempts {
		result, again, err := s.attemptPrivateRepeatable(ctx, store, request)
		if !again {
			return result, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return repeatables.Result{}, ctx.Err()
		case <-timer.C:
		}
	}
	return repeatables.Result{}, errors.New("YDB aborted every attempt to write the repeatable migration record")
}

// attemptPrivateRepeatable runs one attempt and reports whether to run it
// again.
func (s *session) attemptPrivateRepeatable(ctx context.Context, store repeatables.Store, request *protocol.MutateRepeatableRequest) (repeatables.Result, bool, error) {
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return repeatables.Result{}, false, err
	}
	defer func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return repeatables.Result{}, false, err
	}
	rollback := func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			s.cleanupErr = errors.Join(s.cleanupErr, rollbackErr)
		}
	}
	result, err := store.Mutate(ctx, tx, request, s.verifyLocks)
	if err != nil {
		// A statement YDB refused may have ended the transaction on the
		// server already, and the rollback then answers that it is not
		// found; the statement's failure is the one that matters.
		_ = tx.Rollback()
		return repeatables.Result{}, conflicting(err), err
	}
	if result.Outcome != protocol.RepeatableResult_APPLIED {
		rollback()
		return result, false, nil
	}
	if err := s.verifyLocks(ctx); err != nil {
		rollback()
		return repeatables.Result{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return repeatables.Result{}, conflicting(err), err
	}
	return result, false, nil
}

// conflicting reports a failure that proves another transaction got there
// first and that nothing of this one committed.
func conflicting(err error) bool {
	return seeds.Retryable(err) || seeds.KeyConflict(err)
}
