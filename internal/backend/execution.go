package backend

import (
	"context"
	"database/sql"
	"errors"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Execute dispatches each SQL unit once on a pinned connection. Using sql.Conn
// rather than sql.DB avoids database/sql's implicit retry on ErrBadConn. A
// failed dispatched unit has an uncertain effect, even after a confirmed prefix.
func (p *Provider) Execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	if request.GetSessionId() == "" || request.GetRequestId() == "" || len(request.GetUnits()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "session, request ID, and units are required")
	}
	// Validate the whole batch before executing any prefix.
	for _, unit := range request.Units {
		if err := provider.ValidateUnit(unit); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if _, ok := unit.Body.(*protocol.Unit_Sql); !ok {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.unsupported-unit", Message: "Only native SQL execution is implemented"}, nil
		}
	}
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	}
	if err := ctx.Err(); err != nil {
		return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.canceled-before-execution"}, nil
	}
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.lock-guarantee-lost"}, nil
	}
	defer stop()
	ctx = guarded
	var runner sqlExecutor
	if request.TransactionId != "" {
		record := s.transactions[request.TransactionId]
		if record == nil || record.tx == nil || record.failed || s.active != request.TransactionId {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.transaction-inactive"}, nil
		}
		runner = record.tx
	} else {
		if s.active != "" {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.transaction-active"}, nil
		}
		conn, err := s.connection.DB.Conn(ctx)
		if err != nil {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.connection-unavailable"}, nil
		}
		defer func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }()
		runner = conn
	}
	result := executeUnits(ctx, guardedExecutor{session: s, runner: runner}, request.Units)
	if s.verifyLocks(ctx) != nil {
		result.Outcome = protocol.Execution_UNKNOWN
		result.DiagnosticCode = "ydb.lock-guarantee-lost"
	}
	return result, nil
}

func executeUnits(ctx context.Context, runner sqlExecutor, units []*protocol.Unit) *protocol.Execution {
	var applied uint32
	for _, unit := range units {
		if ctx.Err() != nil {
			outcome := protocol.Execution_NOT_STARTED
			if applied != 0 {
				outcome = protocol.Execution_PARTIAL
			}
			return &protocol.Execution{Outcome: outcome, AppliedCount: applied, DiagnosticCode: "ydb.canceled-before-unit"}
		}
		if _, err := runner.ExecContext(ctx, unit.GetSql()); err != nil {
			return &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: applied, DiagnosticCode: "ydb.execution-unconfirmed", Message: "The dispatched unit's outcome is unknown; no automatic retry was attempted"}
		}
		applied++
	}
	return &protocol.Execution{Outcome: protocol.Execution_APPLIED, AppliedCount: applied}
}

func closeSQLConnection(conn *sql.Conn) error {
	if err := conn.Close(); !errors.Is(err, sql.ErrConnDone) {
		return err
	}
	return nil
}

func (p *Provider) lookup(id string) (*session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[id]
	if s == nil {
		return nil, status.Error(codes.NotFound, "unknown YDB session")
	}
	return s, nil
}

// A fresh check precedes each unit, including units after a long earlier query.
type guardedExecutor struct {
	session *session
	runner  sqlExecutor
}

func (g guardedExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := g.session.verifyLocks(ctx); err != nil {
		return nil, err
	}
	return g.runner.ExecContext(ctx, query, args...)
}
