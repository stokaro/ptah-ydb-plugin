package backend

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Execute dispatches SQL and native units once. SQL uses a pinned connection
// to avoid database/sql retries. Native calls validate captured preconditions
// and cannot join a SQL transaction. Unknown outcomes are never retried.
func (p *Provider) Execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	if request.GetSessionId() == "" || request.GetRequestId() == "" || len(request.GetUnits()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "session, request ID, and units are required")
	}
	hasSQL, hasNative := false, false
	// Pure validation covers the whole batch before any prefix can execute.
	for _, unit := range request.Units {
		if err := provider.ValidateUnit(unit); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		switch unit.Body.(type) {
		case *protocol.Unit_Sql:
			hasSQL = true
		case *protocol.Unit_Native:
			hasNative = true
			inputs := &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: Description().Features, Values: []*protocol.ValueInput{{Id: unit.ChangeId, Representation: protocol.ValueInput_OPERATION, Value: unit.GetNative()}}}
			if err := provider.ValidateValueInputs(Description(), inputs); err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.unsupported-unit"}, nil
			}
			if _, err := normalizeOperation(inputs.Values[0]); err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.invalid-native-unit"}, nil
			}
		}
	}
	if hasNative && request.TransactionId != "" {
		return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.native-transaction-forbidden"}, nil
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
	for _, unit := range request.Units {
		if native := unit.GetNative(); native != nil && native.GetOwner().GetDatabase() != "" && native.GetOwner().GetDatabase() != s.database {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.foreign-database"}, nil
		}
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
		if hasSQL {
			conn, err := s.connection.DB.Conn(ctx)
			if err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.connection-unavailable"}, nil
			}
			defer func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }()
			runner = conn
		}
	}
	result := executeUnits(ctx, func(ctx context.Context, unit *protocol.Unit) (bool, error) {
		if unit.GetNative() != nil {
			switch unit.GetNative().GetKind() {
			case tablecreate.OperationKind:
				return nativeapi.NewTableCreation(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.AlterOperationKind:
				return nativeapi.NewTableAlteration(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			}
			return nativeapi.NewCoordination(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
		}
		if err := s.verifyLocks(ctx); err != nil {
			return false, err
		}
		_, err := runner.ExecContext(ctx, unit.GetSql())
		return true, err
	}, request.Units)
	if s.verifyLocks(ctx) != nil && (result.AppliedCount > 0 || result.Outcome == protocol.Execution_UNKNOWN) {
		result.Outcome = protocol.Execution_UNKNOWN
		result.DiagnosticCode = "ydb.lock-guarantee-lost"
	}
	return result, nil
}

func executeUnits(ctx context.Context, run func(context.Context, *protocol.Unit) (bool, error), units []*protocol.Unit) *protocol.Execution {
	var applied uint32
	for _, unit := range units {
		if ctx.Err() != nil {
			outcome := protocol.Execution_NOT_STARTED
			if applied > 0 {
				outcome = protocol.Execution_PARTIAL
			}
			return &protocol.Execution{Outcome: outcome, AppliedCount: applied, DiagnosticCode: "ydb.canceled-before-unit"}
		}
		if dispatched, err := run(ctx, unit); err != nil {
			if !dispatched {
				outcome := protocol.Execution_NOT_STARTED
				if applied > 0 {
					outcome = protocol.Execution_PARTIAL
				}
				return &protocol.Execution{Outcome: outcome, AppliedCount: applied, DiagnosticCode: "ydb.precondition-refused", Message: "The unit was not dispatched because its precondition or lock guarantee was not established"}
			}
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
	s := p.sessions[id]
	p.mu.Unlock()
	if s == nil {
		return nil, status.Error(codes.NotFound, "unknown YDB session")
	}
	return s, nil
}
