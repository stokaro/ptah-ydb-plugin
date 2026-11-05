package backend

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const transactionLimit = 1024

type transaction struct {
	tx      *sql.Tx
	conn    *sql.Conn
	cancel  context.CancelFunc
	outcome protocol.TransactionResult_Outcome
	failed  bool
	// refusal is what YDB answered a commit it aborted with, from which
	// every finish of the handle answers.
	refusal ydbfailure.Answer
}

// BeginTransaction reserves a session-scoped caller handle exactly once.
// A begin RPC's lifetime is distinct from its database transaction lifetime.
func (p *Provider) BeginTransaction(ctx context.Context, request *protocol.BeginTransactionRequest) (*protocol.Transaction, error) {
	if err := provider.ValidateBeginTransactionRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := s.verifyLocks(ctx); err != nil {
		return nil, err
	}
	record, err := s.reserveTransaction(request.TransactionId)
	if err != nil {
		return nil, err
	}
	if s.active != "" {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session has an active transaction")
	}
	ctx, stopBegin := context.WithTimeout(ctx, 30*time.Second)
	defer stopBegin()
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return nil, connectionError(ctx, "acquire YDB transaction connection")
	}
	// The pinned driver maps both supported levels to native serializable
	// read/write. Keep the requested level in the receipt without rewriting it.
	opts := &sql.TxOptions{Isolation: sql.LevelSerializable}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopCancellation := context.AfterFunc(ctx, cancel)
	record.outcome = protocol.TransactionResult_UNKNOWN
	tx, beginErr := conn.BeginTx(lifetime, opts)
	detached := stopCancellation()
	beginContextErr := ctx.Err()
	if beginErr != nil {
		cancel()
		s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn))
		return nil, connectionError(ctx, "begin YDB transaction")
	}
	record.tx, record.conn, record.cancel = tx, conn, cancel
	if !detached || beginContextErr != nil {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		s.finishTransaction(cleanupCtx, record, protocol.FinishTransactionRequest_ROLLBACK)
		if beginContextErr == nil {
			beginContextErr = context.Canceled
		}
		return nil, status.FromContextError(beginContextErr).Err()
	}
	s.active = request.TransactionId
	return &protocol.Transaction{SessionId: request.SessionId, Id: request.TransactionId, Isolation: request.Isolation}, nil
}

func (s *session) reserveTransaction(id string) (*transaction, error) {
	if s.transactions[id] != nil {
		return nil, runtime.ErrorWithReason(codes.AlreadyExists, "YDB transaction handle was already reserved")
	}
	if len(s.transactions) >= transactionLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "YDB transaction handle limit reached; open another session")
	}
	if s.transactions == nil {
		s.transactions = make(map[string]*transaction)
	}
	record := &transaction{outcome: protocol.TransactionResult_NOT_STARTED}
	s.transactions[id] = record
	return record, nil
}

// FinishTransaction returns the retained outcome for a terminal handle. An
// unused handle is fenced, so a rollback arriving before begin cannot be lost.
func (p *Provider) FinishTransaction(ctx context.Context, request *protocol.FinishTransactionRequest) (*protocol.TransactionResult, error) {
	if err := provider.ValidateTransactionRequest(request.GetSessionId(), request.GetTransactionId()); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := provider.ValidateTransactionAction(request.GetAction()); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	record := s.transactions[request.TransactionId]
	if record == nil {
		record, err = s.reserveTransaction(request.TransactionId)
		if err != nil {
			return nil, err
		}
	}
	if record.tx != nil {
		s.closeStatements(request.TransactionId, false)
		s.closeMetadataReaders(request.TransactionId, false)
		if request.Action == protocol.FinishTransactionRequest_COMMIT && !record.failed {
			guarded, stop, err := s.guard(ctx)
			if err != nil {
				return nil, err
			}
			defer stop()
			ctx = guarded
		}
		action := request.Action
		if record.failed {
			action = protocol.FinishTransactionRequest_ROLLBACK
		}
		s.finishTransaction(ctx, record, action)
		if action == protocol.FinishTransactionRequest_COMMIT && s.verifyLocks(ctx) != nil {
			record.outcome, record.refusal = protocol.TransactionResult_UNKNOWN, ydbfailure.Answer{}
		}
		s.active = ""
	}
	if refused := ydbfailure.Commit(record.refusal, slices.Contains(provider.HostFeatures(ctx), provider.MigrationSplitFeature)); refused != nil {
		return refused, nil
	}
	result := &protocol.TransactionResult{Outcome: record.outcome}
	if record.failed && request.Action == protocol.FinishTransactionRequest_COMMIT && record.outcome == protocol.TransactionResult_ROLLED_BACK {
		result.Diagnostic = &protocol.ProviderDiagnostic{Reason: "the transaction was rolled back because a statement in it failed"}
	}
	if record.outcome == protocol.TransactionResult_UNKNOWN {
		result.DiagnosticCode = "ydb.transaction-unconfirmed"
		result.Message = "The transaction outcome is unknown; commit or rollback was not retried"
		result.Diagnostic = &protocol.ProviderDiagnostic{Reason: "YDB did not confirm the transaction's outcome, and the commit or rollback was not retried"}
	}
	return result, nil
}

func (s *session) finishTransaction(ctx context.Context, record *transaction, action protocol.FinishTransactionRequest_Action) {
	bounded, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	stopCancellation := context.AfterFunc(bounded, record.cancel)
	var err error
	if action == protocol.FinishTransactionRequest_COMMIT {
		err = record.tx.Commit()
		record.outcome = protocol.TransactionResult_COMMITTED
	} else {
		err = record.tx.Rollback()
		record.outcome = protocol.TransactionResult_ROLLED_BACK
	}
	stopCancellation()
	record.cancel()
	if err != nil {
		record.outcome = protocol.TransactionResult_UNKNOWN
	}
	if answer := ydbfailure.Read(err); action == protocol.FinishTransactionRequest_COMMIT && ydbfailure.Commit(answer, false) != nil {
		// YDB aborted the transaction for a conflict and applied nothing
		// (see ydbfailure.Commit).
		record.outcome, record.refusal = protocol.TransactionResult_ROLLED_BACK, answer
	}
	s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(record.conn))
	record.tx, record.conn, record.cancel = nil, nil, nil
}

// abandonTransaction rolls back the caller transaction id after YDB stopped
// or refused a unit in it, and retains ROLLED_BACK for the caller's finish.
// YDB has ended the transaction already: measured on 26.2.1.14, a rollback
// after a refused query answers `Transaction not found`, and after a canceled
// one the driver answers that the session is not valid for reuse. The
// transaction never sent a commit, and a YDB transaction is applied only by
// its commit, so it is rolled back either way.
func (s *session) abandonTransaction(ctx context.Context, id string) {
	record := s.transactions[id]
	if record == nil || record.tx == nil {
		return
	}
	record.failed = true
	rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.closeStatements(id, false)
	s.closeMetadataReaders(id, false)
	s.finishTransaction(rollback, record, protocol.FinishTransactionRequest_ROLLBACK)
	record.outcome = protocol.TransactionResult_ROLLED_BACK
	s.active = ""
}

func (s *session) rollbackTransactions() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, record := range s.transactions {
		if record.tx != nil {
			s.finishTransaction(ctx, record, protocol.FinishTransactionRequest_ROLLBACK)
			if record.outcome != protocol.TransactionResult_ROLLED_BACK {
				s.cleanupErr = errors.Join(s.cleanupErr, errors.New("YDB transaction cleanup was not confirmed"))
			}
		}
	}
	s.active = ""
}
