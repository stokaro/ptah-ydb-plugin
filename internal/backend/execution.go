package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Execute dispatches SQL and native units once. SQL uses a pinned connection
// to avoid database/sql retries. Native calls validate captured preconditions
// and cannot join a SQL transaction. Unknown outcomes are never retried. A
// result that did not apply every unit gives its reason in its diagnostic.
func (p *Provider) Execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	result, err := p.execute(ctx, request)
	return withReason(result), err
}

func (p *Provider) execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	if request.GetSessionId() == "" || request.GetRequestId() == "" || len(request.GetUnits()) == 0 {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "session, request ID, and units are required")
	}
	batch, refusal, err := validateBatch(request)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return refusal, nil
	}
	bindings, err := executionBindings(ctx, request)
	if err != nil {
		return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.invalid-execution-inputs", Message: rowdata.OneLine(err.Error())}, nil
	}
	defer clear(bindings)
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
	}
	if refused := s.refuseUnits(request); refused != nil {
		return refused, nil
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
	runner, release, refusal := s.runnerFor(ctx, request, batch)
	if refusal != nil {
		return refusal, nil
	}
	defer release()
	stopped := s.timedOut(ctx, request)
	failed := s.unitFailure(ctx, request, batch.services)
	// The statement timeout bounds each unit's statements, not the check of
	// the session's locks before them. checkLock fences the session on any
	// failed check, so a timeout that ended during the check would fence it as
	// if the lock were lost, and the unit would not be sent at all. The checks
	// run under the request's context, which a lost lock ends (see guard).
	checkLocks := func(context.Context) error { return s.verifyLocks(ctx) }
	result := executeUnits(ctx, time.Duration(request.GetStatementTimeoutNanoseconds()), stopped, failed, s.unitRunner(runner, batch, bindings, checkLocks), request.Units)
	if s.verifyLocks(ctx) != nil && (result.AppliedCount > 0 || result.Outcome == protocol.Execution_UNKNOWN) {
		result.Outcome = protocol.Execution_UNKNOWN
		result.DiagnosticCode = "ydb.lock-guarantee-lost"
		// The loss decides the outcome, so the reason YDB's answer gave for
		// the unit no longer explains it.
		result.Diagnostic = nil
	}
	return result, nil
}

// executionBatch is what the validation of a request learns of its units.
type executionBatch struct {
	hasSQL, hasNative bool
	// services are the SQL units that run one of Ptah's comment or
	// coordination node statements, which YDB's query service does not take,
	// cut into the queries they run as.
	services map[*protocol.Unit][]sqlPiece
}

// validateBatch validates the whole batch before any prefix can execute, and
// reads what running it needs. A batch the provider refuses returns the
// execution that says why, with nothing started; a unit the SDK refuses is an
// error.
func validateBatch(request *protocol.ExecuteRequest) (*executionBatch, *protocol.Execution, error) {
	batch := &executionBatch{services: make(map[*protocol.Unit][]sqlPiece)}
	for _, unit := range request.Units {
		if err := provider.ValidateUnit(unit); err != nil {
			return nil, nil, authored.Status(codes.InvalidArgument, err)
		}
		switch unit.Body.(type) {
		case *protocol.Unit_Sql:
			batch.hasSQL = true
			pieces, err := unitPieces(unit.GetSql())
			switch {
			case err != nil:
				code := "ydb.invalid-coordination-statement"
				if errors.Is(err, ydbcomment.ErrStatement) {
					code = "ydb.invalid-comment-statement"
				}
				return nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: code, Message: rowdata.OneLine(err.Error())}, nil
			case pieces != nil && request.TransactionId != "":
				return nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.native-transaction-forbidden",
					Message: "A comment or coordination node statement runs outside a transaction, as YDB runs every scheme statement"}, nil
			case pieces != nil:
				batch.services[unit] = pieces
			}
		case *protocol.Unit_Native:
			batch.hasNative = true
			inputs := &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: Description().Features, Values: []*protocol.ValueInput{{Id: unit.ChangeId, Representation: protocol.ValueInput_OPERATION, Value: unit.GetNative()}}}
			if err := provider.ValidateValueInputs(Description(), inputs); err != nil {
				return nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.unsupported-unit"}, nil
			}
			if _, err := normalizeOperation(inputs.Values[0]); err != nil {
				return nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.invalid-native-unit"}, nil
			}
		}
	}
	if batch.hasNative && request.TransactionId != "" {
		return nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.native-transaction-forbidden"}, nil
	}
	return batch, nil, nil
}

// refuseUnits returns the execution, with nothing started, that refuses a
// unit the session cannot run: a secret the target does not support, a column
// addition its capabilities refuse, an object of another database, and in a
// dev realm a unit whose effects the realm cannot confine. It returns nil when
// the session can run every unit.
func (s *session) refuseUnits(request *protocol.ExecuteRequest) *protocol.Execution {
	if !s.capabilities["secrets"] {
		for _, unit := range request.Units {
			if unit.GetNative().GetKind() == schema.SecretOperation || definesSecret(unit.GetSql()) {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.secrets-unsupported"}
			}
		}
	}
	for _, unit := range request.Units {
		if unit.GetNative().GetKind() == tablecreate.InventoryOperationKind {
			change, err := tablecreate.DecodeInventory(unit.GetNative())
			if err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.invalid-native-unit"}
			}
			if err := change.CheckCapabilities(s.capabilities); err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.column-addition-unsupported", Message: err.Error()}
			}
		}
	}
	for _, unit := range request.Units {
		if native := unit.GetNative(); native != nil && native.GetOwner().GetDatabase() != "" && native.GetOwner().GetDatabase() != s.database {
			return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.foreign-database"}
		}
	}
	if s.realm {
		for _, unit := range request.Units {
			if err := confineToRealm(unit); err != nil {
				return &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.realm-escape", Message: err.Error()}
			}
		}
	}
	return nil
}

// runnerFor returns what runs the request's SQL: the active transaction the
// request names, or a pinned connection of its own when the batch holds SQL
// and none when it holds none. release gives a pinned connection back. A
// request the session cannot run returns the execution that refuses it, with
// nothing started.
func (s *session) runnerFor(ctx context.Context, request *protocol.ExecuteRequest, batch *executionBatch) (runner sqlExecutor, release func(), refusal *protocol.Execution) {
	if request.TransactionId != "" {
		record := s.transactions[request.TransactionId]
		if record == nil || record.tx == nil || record.failed || s.active != request.TransactionId {
			return nil, nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.transaction-inactive"}
		}
		return record.tx, func() {}, nil
	}
	if s.active != "" {
		return nil, nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.transaction-active"}
	}
	if !batch.hasSQL {
		return nil, func() {}, nil
	}
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		return nil, nil, &protocol.Execution{Outcome: protocol.Execution_NOT_STARTED, DiagnosticCode: "ydb.connection-unavailable"}
	}
	return conn, func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }, nil
}

// unitFailure returns the answer for a unit of the request that failed after
// dispatch, read from YDB's answer and from what the unit's text proves. A
// failure that leaves the outcome known abandons the request's transaction.
func (s *session) unitFailure(ctx context.Context, request *protocol.ExecuteRequest, services map[*protocol.Unit][]sqlPiece) failedUnit {
	aborts := slices.Contains(provider.HostFeatures(ctx), provider.MigrationSplitFeature)
	return func(unit *protocol.Unit, err error, applied uint32) *protocol.Execution {
		failure := ydbfailure.Unit{Text: unit.GetSql(), Transaction: request.TransactionId != "", Aborts: aborts}
		if _, split := services[unit]; split {
			// A comment or coordination node statement runs outside the
			// query service, beside the queries the unit holds, so the
			// unit's text says nothing of what failed. A failure of its first
			// piece left nothing of the unit applied: the piece's own answer
			// says what it proves.
			failure.Text = ""
			if first, ok := errors.AsType[*firstPieceFailure](err); ok {
				switch {
				case first.piece.comment != nil:
					failure.Call = true
				case !first.piece.service():
					failure.Text = first.piece.text
				}
			}
		}
		if first, ok := errors.AsType[*firstPieceFailure](err); ok && unit.GetNative() != nil {
			// A native unit whose first statement failed applied nothing
			// of itself, and YDB's answer to that statement says what it
			// proves: a DROP YDB refuses because other objects depend on
			// what it drops is NOT_APPLIED.
			failure.Text = first.piece.text
		}
		answer := ydbfailure.Read(err)
		answer.FlagsRead = s.connection.HasMonitoring()
		if s.realm {
			answer = answer.WithoutLines(realmPrefixLines)
		}
		result := ydbfailure.Failed(answer, failure, applied)
		if result.GetOutcome() != protocol.Execution_UNKNOWN && request.TransactionId != "" {
			s.abandonTransaction(ctx, request.TransactionId)
		}
		return result
	}
}

// unitRunner returns what runs one unit once: a native unit through its
// service, a SQL unit cut into pieces piece by piece, and any other SQL unit
// through runner. Input references are expanded from bindings, and checkLocks
// verifies the session's locks right before a unit is sent.
func (s *session) unitRunner(runner sqlExecutor, batch *executionBatch, bindings map[string]string, checkLocks func(context.Context) error) func(context.Context, *protocol.Unit) (bool, error) {
	return func(ctx context.Context, unit *protocol.Unit) (bool, error) {
		if unit.GetNative() != nil {
			return s.applyNative(ctx, unit.GetNative(), bindings, checkLocks, &firstStatement{})
		}
		if pieces, split := batch.services[unit]; split {
			return s.runPieces(ctx, runner, pieces, bindings, checkLocks)
		}
		return s.runInputPiece(ctx, runner, sqlPiece{text: unit.GetSql()}, bindings, checkLocks)
	}
}

// applyNative applies one native unit on a table through the service of its
// kind, and leaves every other unit to applyObject.
//
// Every service but the column inventory and the rebuild copy writes only
// through the statements it sends, after it has read and checked, so first
// marks the failure of the first one: it leaves nothing of the unit applied.
// The column inventory alters a table through the table service before its
// statements, and the copy keeps the answer it gives today.
func (s *session) applyNative(ctx context.Context, native *protocol.Envelope, bindings map[string]string, checkLocks func(context.Context) error,
	first *firstStatement,
) (bool, error) {
	execute := first.wrap(s.executeSchema)
	switch native.GetKind() {
	case tablecreate.OperationKind:
		return nativeapi.NewTableCreation(s.connection.Driver).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.InventoryOperationKind:
		return nativeapi.NewColumnInventory(s.connection.Driver, s.executeSchema).Apply(ctx, s.database, native, s.capabilities, checkLocks)
	case tablecreate.DefaultOperationKind:
		return nativeapi.NewColumnDefault(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.AlterOperationKind:
		return nativeapi.NewTableAlteration(s.connection.Driver).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.IndexSettingsOperationKind:
		return nativeapi.NewIndexSettings(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.ColumnOperationKind:
		return nativeapi.NewColumnTableCreation(s.connection.Driver, execute, s.columnTables()).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.ColumnTTLOperationKind:
		return nativeapi.NewColumnTableTTL(s.connection.Driver, execute, s.columnTables()).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.ColumnLocalIndexOperationKind:
		return nativeapi.NewColumnTableLocalIndex(s.connection.Driver, execute, s.columnTables()).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.DropOperationKind:
		return nativeapi.NewTableDrop(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.CopyOperationKind:
		return nativeapi.NewTableStatements(s.connection.Driver, s.executeSchema, s.connection.DB).ApplyCopy(ctx, s.database, native, checkLocks)
	case tablecreate.RenameOperationKind:
		return nativeapi.NewTableStatements(s.connection.Driver, execute, s.connection.DB).ApplyRename(ctx, s.database, native, checkLocks)
	case tablecreate.NativeIndexOperationKind:
		return nativeapi.NewNativeIndexAddition(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case tablecreate.SequenceOperationKind:
		return nativeapi.NewSerialSequence(s.connection.Driver, execute).Apply(ctx, s.database, native, s.capabilities, checkLocks)
	default:
		return s.applyObject(ctx, native, bindings, checkLocks, first)
	}
}

// applyObject applies one native unit on an object other than a table through
// the service of its kind, and a unit of any other kind as a coordination node
// operation.
func (s *session) applyObject(ctx context.Context, native *protocol.Envelope, bindings map[string]string, checkLocks func(context.Context) error,
	first *firstStatement,
) (bool, error) {
	execute := first.wrap(s.executeSchema)
	switch native.GetKind() {
	case schema.SecretOperation:
		if !s.capabilities["secrets"] {
			return false, fmt.Errorf("the target does not support scheme secrets")
		}
		// The first statement is marked as written, before its inputs are
		// expanded, so the failure holds no secret value.
		expanded := first.wrap(func(ctx context.Context, query string) error {
			expansion, err := expandInputs(query, bindings)
			if err != nil {
				return err
			}
			return inputError(expansion, s.executeSchema(ctx, expansion.Text))
		})
		return nativeapi.NewSecret(s.connection.Driver, expanded).Apply(ctx, s.database, native, checkLocks)
	case schema.TopicOperation:
		return nativeapi.NewTopic(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case schema.ChangefeedOperation:
		return nativeapi.NewChangefeed(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case schema.ExternalSourceOperation, schema.ExternalTableOperation:
		return nativeapi.NewExternal(s.connection.Driver, execute).Apply(ctx, s.database, native, checkLocks)
	case schema.StreamingQueryOperation:
		read := s.streaming()
		if read == nil {
			return false, fmt.Errorf("streaming queries require an established capability")
		}
		return (&nativeapi.StreamingQuery{Read: read, Execute: execute}).Apply(ctx, s.database, native, checkLocks)
	case schema.ReplicationOperation:
		return nativeapi.NewReplications(s.connection.Driver, execute).ApplyReplication(ctx, s.database, native, checkLocks)
	case schema.TransferOperation:
		return nativeapi.NewReplications(s.connection.Driver, execute).ApplyTransfer(ctx, s.database, native, checkLocks)
	case schema.WorkloadOperation:
		read, unread := s.workload()
		if read == nil {
			return false, fmt.Errorf("resource pools cannot change here: %s", unread)
		}
		return (&nativeapi.Workload{Read: read, Execute: execute}).Apply(ctx, s.database, native, checkLocks)
	}
	return nativeapi.NewCoordination(s.connection.Driver).Apply(ctx, s.database, native, checkLocks)
}

// runPieces runs the pieces of a SQL unit in order and stops at the first
// that fails. A failure of the first piece is a firstPieceFailure.
func (s *session) runPieces(ctx context.Context, runner sqlExecutor, pieces []sqlPiece, bindings map[string]string, checkLocks func(context.Context) error) (bool, error) {
	for i, piece := range pieces {
		if dispatched, err := s.runInputPiece(ctx, runner, piece, bindings, checkLocks); err != nil {
			// A piece after the first leaves the unit partly applied.
			if i == 0 {
				err = &firstPieceFailure{piece: piece, err: err}
			}
			return dispatched || i > 0, err
		}
	}
	return true, nil
}

// firstPieceFailure is the failure of the first piece of a SQL unit cut into
// pieces, or of the first statement a native unit's service sent, which
// leaves nothing of the unit applied.
type firstPieceFailure struct {
	piece sqlPiece
	err   error
}

// firstStatement marks the failure of the first statement a native unit's
// service sends as a firstPieceFailure holding that statement as written.
type firstStatement struct{ sent bool }

// wrap returns run with the failure of the first statement it runs marked.
func (f *firstStatement) wrap(run func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, statement string) error {
		first := !f.sent
		f.sent = true
		err := run(ctx, statement)
		if err != nil && first {
			return &firstPieceFailure{piece: sqlPiece{text: statement}, err: err}
		}
		return err
	}
}

func (f *firstPieceFailure) Error() string { return f.err.Error() }
func (f *firstPieceFailure) Unwrap() error { return f.err }

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
// service, and every other query through runner. checkLocks verifies the
// session's locks right before the piece is sent.
func (s *session) runPiece(ctx context.Context, runner sqlExecutor, piece sqlPiece, checkLocks func(context.Context) error) (bool, error) {
	switch {
	case piece.coordination != nil:
		return nativeapi.NewCoordination(s.connection.Driver).RunStatement(ctx, s.connection.Database(), s.database, *piece.coordination, checkLocks)
	case piece.comment != nil:
		return nativeapi.NewComments(s.connection.Driver).RunStatement(ctx, s.connection.Database(), s.database, *piece.comment, checkLocks)
	}
	if err := checkLocks(ctx); err != nil {
		return false, err
	}
	_, err := runner.ExecContext(ctx, piece.text)
	return true, err
}

// confineToRealm refuses a unit whose effects a dev realm cannot confine: a
// SQL statement the realm guard refuses, and an external data source or
// external table operation, which reads past the server as such a statement
// does.
func confineToRealm(unit *protocol.Unit) error {
	switch unit.GetNative().GetKind() {
	case schema.ExternalSourceOperation:
		return &yql.RealmRefusal{Operation: "an external data source"}
	case schema.ExternalTableOperation:
		return &yql.RealmRefusal{Operation: "an external table"}
	case schema.ReplicationOperation:
		return &yql.RealmRefusal{Operation: "an async replication"}
	case schema.TransferOperation:
		return &yql.RealmRefusal{Operation: "a transfer"}
	case schema.StreamingQueryOperation:
		change, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(unit.GetNative()))
		if err != nil {
			return err
		}
		statements, err := streaming.Statements(change)
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if err := yql.ConfineToRealm(statement); err != nil {
				return err
			}
		}
	}
	return yql.ConfineToRealm(unit.GetSql())
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

// failedUnit answers for a unit that failed after it was dispatched, given the
// error and the count of units applied before it.
type failedUnit func(unit *protocol.Unit, err error, applied uint32) *protocol.Execution

// executeUnits runs units in order and stops at the first that fails. A
// positive timeout bounds each unit; a unit it stops is answered for by
// stopped and marked statement_timed_out. A unit that fails otherwise after
// dispatch is answered for by failed.
func executeUnits(ctx context.Context, timeout time.Duration, stopped stoppedUnit, failed failedUnit, run func(context.Context, *protocol.Unit) (bool, error), units []*protocol.Unit) *protocol.Execution {
	var applied uint32
	for _, unit := range units {
		if ctx.Err() != nil {
			return &protocol.Execution{Outcome: partialOr(protocol.Execution_NOT_STARTED, applied), AppliedCount: applied, DiagnosticCode: "ydb.canceled-before-unit"}
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
				return &protocol.Execution{Outcome: partialOr(protocol.Execution_NOT_APPLIED, applied), AppliedCount: applied, StatementTimedOut: true, DiagnosticCode: "ydb.statement-timeout",
					Message: "The statement timeout stopped the unit, and nothing of it was applied"}
			default:
				return &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: applied, StatementTimedOut: true, DiagnosticCode: "ydb.statement-timeout",
					Message: "The statement timeout stopped waiting for the unit, which YDB may still apply; no automatic retry was attempted"}
			}
		}
		if err != nil {
			if !dispatched {
				return notDispatched(err, applied)
			}
			return failed(unit, err, applied)
		}
		applied++
	}
	return &protocol.Execution{Outcome: protocol.Execution_APPLIED, AppliedCount: applied}
}

// partialOr is outcome for a batch that applied no unit before it stopped,
// and PARTIAL for one that applied some.
func partialOr(outcome protocol.Execution_Outcome, applied uint32) protocol.Execution_Outcome {
	if applied > 0 {
		return protocol.Execution_PARTIAL
	}
	return outcome
}

// notDispatched answers for a unit whose precondition or lock guarantee was
// not established, so it was never sent, after applied units. A refusal's own
// words are the message.
func notDispatched(err error, applied uint32) *protocol.Execution {
	message := "The unit was not dispatched because its precondition or lock guarantee was not established"
	if refusal, ok := errors.AsType[*nativeapi.Refusal](err); ok {
		message = rowdata.OneLine(refusal.Error())
	}
	return &protocol.Execution{Outcome: partialOr(protocol.Execution_NOT_STARTED, applied), AppliedCount: applied, DiagnosticCode: "ydb.precondition-refused", Message: message}
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
		return nil, runtime.ErrorWithReason(codes.NotFound, "unknown YDB session")
	}
	return s, nil
}

// runInputPiece expands references only in the transient text passed to YDB.
func (s *session) runInputPiece(ctx context.Context, runner sqlExecutor, piece sqlPiece, bindings map[string]string, checkLocks func(context.Context) error) (bool, error) {
	if piece.service() {
		return s.runPiece(ctx, runner, piece, checkLocks)
	}
	expansion, err := expandInputs(piece.text, bindings)
	if err != nil {
		return false, err
	}
	if expansion.DefinesSecrets() && !s.capabilities["secrets"] {
		return false, fmt.Errorf("the target does not support scheme secrets")
	}
	piece.text = expansion.Text
	dispatched, err := s.runPiece(ctx, runner, piece, checkLocks)
	return dispatched, inputError(expansion, err)
}
