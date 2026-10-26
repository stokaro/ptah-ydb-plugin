package backend

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	// services are the SQL units that run one of Ptah's comment or
	// coordination node statements, which YDB's query service does not take,
	// cut into the queries they run as.
	services := make(map[*protocol.Unit][]sqlPiece)
	// Pure validation covers the whole batch before any prefix can execute.
	for _, unit := range request.Units {
		if err := provider.ValidateUnit(unit); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		switch unit.Body.(type) {
		case *protocol.Unit_Sql:
			hasSQL = true
			pieces, err := unitPieces(unit.GetSql())
			switch {
			case err != nil:
				code := "ydb.invalid-coordination-statement"
				if errors.Is(err, ydbcomment.ErrStatement) {
					code = "ydb.invalid-comment-statement"
				}
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: code, Message: rowdata.OneLine(err.Error())}, nil
			case pieces != nil && request.TransactionId != "":
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.native-transaction-forbidden",
					Message: "A comment or coordination node statement runs outside a transaction, as YDB runs every scheme statement"}, nil
			case pieces != nil:
				services[unit] = pieces
			}
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
	if s.realm {
		for _, unit := range request.Units {
			if err := yql.ConfineToRealm(unit.GetSql()); err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.realm-escape", Message: err.Error()}, nil
			}
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
	stopped := s.timedOut(ctx, request)
	result := executeUnits(ctx, time.Duration(request.GetStatementTimeoutNanoseconds()), stopped, func(ctx context.Context, unit *protocol.Unit) (bool, error) {
		if unit.GetNative() != nil {
			switch unit.GetNative().GetKind() {
			case tablecreate.OperationKind:
				return nativeapi.NewTableCreation(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.AlterOperationKind:
				return nativeapi.NewTableAlteration(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.ColumnOperationKind:
				return nativeapi.NewColumnTableCreation(s.connection.Driver, s.executeSchema, s.columnTables()).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.ColumnTTLOperationKind:
				return nativeapi.NewColumnTableTTL(s.connection.Driver, s.executeSchema, s.columnTables()).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case schema.TopicOperation:
				return nativeapi.NewTopic(s.connection.Driver, s.executeSchema).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.DropOperationKind:
				return nativeapi.NewTableDrop(s.connection.Driver, s.executeSchema).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.CopyOperationKind:
				return nativeapi.NewTableStatements(s.connection.Driver, s.executeSchema, s.connection.DB).ApplyCopy(ctx, s.database, unit.GetNative(), s.verifyLocks)
			case tablecreate.RenameOperationKind:
				return nativeapi.NewTableStatements(s.connection.Driver, s.executeSchema, s.connection.DB).ApplyRename(ctx, s.database, unit.GetNative(), s.verifyLocks)
			}
			return nativeapi.NewCoordination(s.connection.Driver).Apply(ctx, s.database, unit.GetNative(), s.verifyLocks)
		}
		if pieces, split := services[unit]; split {
			for i, piece := range pieces {
				if dispatched, err := s.runPiece(ctx, runner, piece); err != nil {
					// A piece after the first leaves the unit partly applied.
					return dispatched || i > 0, err
				}
			}
			return true, nil
		}
		return s.runPiece(ctx, runner, sqlPiece{text: unit.GetSql()})
	}, request.Units)
	if s.verifyLocks(ctx) != nil && (result.AppliedCount > 0 || result.Outcome == protocol.Execution_UNKNOWN) {
		result.Outcome = protocol.Execution_UNKNOWN
		result.DiagnosticCode = "ydb.lock-guarantee-lost"
	}
	return result, nil
}

// sqlPiece is one query a SQL unit runs as: a comment statement, a
// coordination node statement, or a query YDB's query service runs.
type sqlPiece struct {
	text         string
	coordination *coordinationsql.Query
	comment      *ydbcomment.Query
}

// unitPieces cuts a SQL unit that runs one of Ptah's comment or coordination
// node statements into the queries it runs as, in order, or returns nil for a
// unit that runs none and goes to the query service whole.
//
// A unit that is one such statement, preceded by nothing but definitions, is
// one piece. A unit that holds one beside other statements, such as the CREATE
// VIEW and the COMMENT ON VIEW a commented view renders as, is cut as
// internal/yqlquery cuts a migration file: each scheme statement a query of
// its own, with the definitions above it. A statement Ptah cannot read is an
// error wrapping the ErrStatement of its package.
func unitPieces(text string) ([]sqlPiece, error) {
	whole, err := recognizePiece(text)
	if err == nil && whole.service() {
		return []sqlPiece{whole}, nil
	}
	if err == nil && !servicequery.ContainsFold(text, "comment") && !servicequery.ContainsFold(text, "coordination") {
		return nil, nil
	}
	queries, splitErr := yqlquery.Split(text)
	if splitErr != nil {
		return nil, err
	}
	pieces := make([]sqlPiece, 0, len(queries))
	services := 0
	for _, query := range queries {
		piece, err := recognizePiece(query.Text)
		if err != nil {
			return nil, err
		}
		if piece.service() {
			services++
		}
		pieces = append(pieces, piece)
	}
	if services == 0 {
		return nil, err
	}
	return pieces, nil
}

// recognizePiece reads one query: a comment statement, a coordination node
// statement, or neither.
func recognizePiece(text string) (sqlPiece, error) {
	coordination, recognized, err := coordinationsql.Recognize(text)
	switch {
	case err != nil:
		return sqlPiece{}, err
	case recognized:
		return sqlPiece{text: text, coordination: &coordination}, nil
	}
	comment, recognized, err := ydbcomment.Recognize(text)
	switch {
	case err != nil:
		return sqlPiece{}, err
	case recognized:
		return sqlPiece{text: text, comment: &comment}, nil
	}
	return sqlPiece{text: text}, nil
}

// service reports a piece YDB's query service does not take.
func (p sqlPiece) service() bool { return p.coordination != nil || p.comment != nil }

// runPiece runs one piece of a SQL unit once: a comment statement through the
// table service, a coordination node statement through the coordination
// service, and every other query through runner.
func (s *session) runPiece(ctx context.Context, runner sqlExecutor, piece sqlPiece) (bool, error) {
	switch {
	case piece.coordination != nil:
		return nativeapi.NewCoordination(s.connection.Driver).RunStatement(ctx, s.connection.Database(), s.database, *piece.coordination, s.verifyLocks)
	case piece.comment != nil:
		return nativeapi.NewComments(s.connection.Driver).RunStatement(ctx, s.connection.Database(), s.database, *piece.comment, s.verifyLocks)
	}
	if err := s.verifyLocks(ctx); err != nil {
		return false, err
	}
	_, err := runner.ExecContext(ctx, piece.text)
	return true, err
}

// executeSchema runs one schema statement on a connection of its own, once.
// A pinned connection keeps database/sql from replaying it on another.
func (s *session) executeSchema(ctx context.Context, statement string) error {
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, statement)
	return errors.Join(err, closeSQLConnection(conn))
}

// stoppedUnit answers for a unit the statement timeout stopped: whether
// nothing of it was applied, it was applied after all, or what became of it is
// unknown.
type stoppedUnit func(*protocol.Unit) stopOutcome

type stopOutcome uint8

const (
	// stopUnknown is a unit that may still be applied.
	stopUnknown stopOutcome = iota
	// stopNotApplied is a unit of which nothing was applied.
	stopNotApplied
	// stopApplied is a unit that completed after all.
	stopApplied
)

// executeUnits runs units in order and stops at the first that fails. A
// positive timeout bounds each unit; a unit it stops is answered for by
// stopped and marked statement_timed_out.
func executeUnits(ctx context.Context, timeout time.Duration, stopped stoppedUnit, run func(context.Context, *protocol.Unit) (bool, error), units []*protocol.Unit) *protocol.Execution {
	var applied uint32
	for _, unit := range units {
		if ctx.Err() != nil {
			outcome := protocol.Execution_NOT_STARTED
			if applied > 0 {
				outcome = protocol.Execution_PARTIAL
			}
			return &protocol.Execution{Outcome: outcome, AppliedCount: applied, DiagnosticCode: "ydb.canceled-before-unit"}
		}
		unitCtx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			unitCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		dispatched, err := run(unitCtx, unit)
		timedOut := err != nil && dispatched && timeout > 0 && errors.Is(unitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if timedOut {
			switch stopped(unit) {
			case stopApplied:
				applied++
				continue
			case stopNotApplied:
				outcome := protocol.Execution_NOT_APPLIED
				if applied > 0 {
					outcome = protocol.Execution_PARTIAL
				}
				return &protocol.Execution{Outcome: outcome, AppliedCount: applied, StatementTimedOut: true, DiagnosticCode: "ydb.statement-timeout",
					Message: "The statement timeout stopped the unit, and nothing of it was applied"}
			default:
				return &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: applied, StatementTimedOut: true, DiagnosticCode: "ydb.statement-timeout",
					Message: "The statement timeout stopped waiting for the unit, which YDB may still apply; no automatic retry was attempted"}
			}
		}
		if err != nil {
			if !dispatched {
				outcome := protocol.Execution_NOT_STARTED
				if applied > 0 {
					outcome = protocol.Execution_PARTIAL
				}
				message := "The unit was not dispatched because its precondition or lock guarantee was not established"
				if refusal, ok := errors.AsType[*nativeapi.StatementRefusal](err); ok {
					message = rowdata.OneLine(refusal.Error())
				}
				return &protocol.Execution{Outcome: outcome, AppliedCount: applied, DiagnosticCode: "ydb.precondition-refused", Message: message}
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
