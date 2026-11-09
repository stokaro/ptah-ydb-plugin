package backend

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/readproof"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/internal/statementrows"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

const statementLimit = 1024
const activeStatementLimit = 16

type statementResult struct {
	cursor      *statementrows.Cursor
	transaction string
	conn        *sql.Conn
	readOnly    *sql.Tx
	cancel      context.CancelFunc
	closed      bool
	closeErr    error
	columns     uint32
}

var _ provider.StatementRunner = (*Provider)(nil)

// RunStatement executes an operator's statement once on a pinned connection.
// A read-only request first proves its local reads and opens a snapshot
// read-only transaction. YDB reports no affected-row count.
func (p *Provider) RunStatement(ctx context.Context, request *protocol.RunStatementRequest) (*protocol.StatementResult, error) {
	if err := provider.ValidateRunStatementRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	refuse := func(reason string) (*protocol.StatementResult, error) {
		return refusedStatement(request.RequestId, reason), nil
	}
	piece, arguments, reason := readStatement(ctx, request)
	if reason != "" {
		return refuse(reason)
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
	if reason := s.statementRefusal(request); reason != "" {
		return refuse(reason)
	}
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return refuse("statement lock guarantee is unavailable")
	}
	defer stop()
	ctx = guarded
	if request.Access == protocol.RunStatementRequest_READ_ONLY {
		if err := readproof.ProveLocalReads(ctx, readproof.Catalog{Driver: s.connection.Driver}, s.database, request.Sql); err != nil {
			return refuse(err.Error())
		}
	}
	if ctx.Err() != nil {
		return refuse("statement was canceled before execution")
	}
	if piece.service() {
		return s.runServiceStatement(ctx, request, piece), nil
	}
	// The RPC cancellation controls work while this call is active. A returned
	// cursor owns its lifetime until a later page, close, or transaction end.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	detach := context.AfterFunc(ctx, cancel)
	defer detach()
	record := &statementResult{transaction: request.TransactionId, cancel: cancel}
	runner, reason := s.statementRunner(ctx, lifetime, request, record)
	if reason != "" {
		return refuse(reason)
	}
	var statementErr error
	record.cursor, statementErr, err = statementrows.Query(lifetime, runner, request.Sql, arguments...)
	if statementErr != nil {
		s.failStatement(record)
		return statementFailure(request.RequestId, statementErr, request.Sql), nil
	}
	if err != nil {
		s.failStatement(record)
		return nil, authored.Status(codes.FailedPrecondition, err)
	}
	result := &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_SUCCEEDED, Columns: record.cursor.Columns()}
	record.columns = uint32(len(result.Columns)) // #nosec G115 -- the columns go out in this reply, and runtime.MaxMessageBytes bounds a reply to 4 MiB
	var more bool
	result.Rows, more, err = record.cursor.Page(request.MaxRows)
	if err != nil {
		s.failStatement(record)
		return statementFailure(request.RequestId, err, request.Sql), nil
	}
	if ctx.Err() != nil || s.verifyLocks(ctx) != nil {
		s.failStatement(record)
		return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_UNKNOWN}, nil
	}
	if more {
		s.keepStatement(result, record)
	} else if err := s.releaseStatement(record); err != nil {
		return nil, err
	}
	return result, nil
}

// refusedStatement is the result of a statement RunStatement refuses for
// reason, which it writes on one line.
func refusedStatement(requestID, reason string) *protocol.StatementResult {
	return &protocol.StatementResult{RequestId: requestID, Outcome: protocol.StatementResult_REFUSED, Refusal: rowdata.OneLine(reason)}
}

// readStatement reads a request's statement and its bound arguments before
// anything is opened for it, and returns why RunStatement refuses them, or ""
// when it does not; see statementText. A comment or coordination statement
// takes no transaction and no bound argument.
func readStatement(ctx context.Context, request *protocol.RunStatementRequest) (sqlPiece, []any, string) {
	piece, reason := statementText(ctx, request.Sql, request.Access)
	if reason != "" {
		return sqlPiece{}, nil, reason
	}
	if piece.service() && (request.TransactionId != "" || len(request.Arguments) != 0 || len(request.TypedArguments) != 0) {
		return sqlPiece{}, nil, "a comment or coordination statement runs without a transaction or bound arguments"
	}
	arguments, err := statementArguments(request)
	if err != nil {
		return sqlPiece{}, nil, err.Error()
	}
	return piece, arguments, ""
}

// statementRefusal returns why the session refuses to run the request's
// statement now, or "" when it does not: a statement a dev realm cannot
// confine, a session at its result limit, and a transaction the request names
// that is not the active one, or none named while one is active.
func (s *session) statementRefusal(request *protocol.RunStatementRequest) string {
	if s.realm {
		if err := yql.ConfineToRealm(request.Sql); err != nil {
			return err.Error()
		}
	}
	if len(s.statementResults) >= statementLimit || s.activeStatements() >= activeStatementLimit {
		return "statement result limit reached; close results or open another session"
	}
	if request.TransactionId == "" && s.active != "" {
		return "a transaction is active on this session"
	}
	if request.TransactionId != "" {
		tx := s.transactions[request.TransactionId]
		if tx == nil || tx.tx == nil || tx.failed || s.active != request.TransactionId {
			return "statement transaction is inactive"
		}
	}
	return ""
}

// runServiceStatement runs a comment or coordination statement once, outside
// the query service, and answers for it. A statement that was never sent is
// refused; one whose locks or context did not hold to the end is UNKNOWN.
func (s *session) runServiceStatement(ctx context.Context, request *protocol.RunStatementRequest, piece sqlPiece) *protocol.StatementResult {
	dispatched, err := s.runPiece(ctx, nil, piece, s.verifyLocks)
	if err != nil {
		if !dispatched {
			return refusedStatement(request.RequestId, err.Error())
		}
		return statementFailure(request.RequestId, err, request.Sql)
	}
	if ctx.Err() != nil || s.verifyLocks(ctx) != nil {
		return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_UNKNOWN}
	}
	return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_SUCCEEDED}
}

// statementRunner returns what runs the request's statement: the transaction
// it names, or a connection pinned to record, inside a snapshot read-only
// transaction opened for lifetime when the request is READ_ONLY. When the
// connection or that transaction does not open, it ends record's lifetime and
// returns why RunStatement refuses the statement.
func (s *session) statementRunner(ctx, lifetime context.Context, request *protocol.RunStatementRequest, record *statementResult) (rowReader, string) {
	if request.TransactionId != "" {
		return s.transactions[request.TransactionId].tx, ""
	}
	var err error
	record.conn, err = s.connection.DB.Conn(ctx)
	if err != nil {
		record.cancel()
		return nil, "statement connection is unavailable"
	}
	if request.Access != protocol.RunStatementRequest_READ_ONLY {
		return record.conn, ""
	}
	record.readOnly, err = record.conn.BeginTx(lifetime, &sql.TxOptions{Isolation: sql.LevelSnapshot, ReadOnly: true})
	if err != nil {
		_ = s.releaseStatement(record)
		return nil, "cannot open a snapshot read-only transaction"
	}
	return record.readOnly, ""
}

// keepStatement keeps a result that has more rows under a fresh page token,
// which it writes on result.
func (s *session) keepStatement(result *protocol.StatementResult, record *statementResult) {
	if s.statementResults == nil {
		s.statementResults = make(map[string]*statementResult)
	}
	result.NextPageToken = rand.Text()
	s.statementResults[result.NextPageToken] = record
}

// statementText reads an operator's statement before anything is opened for
// it, and returns why RunStatement refuses its text, or "" when it does not: a
// reference to a secret, which only approved execution inputs may carry; a
// comment or coordination statement Ptah cannot read; text that is not
// exactly one statement; and, under READ_ONLY, text [readproof.ReadOnly]
// does not prove one read-only SELECT. A migration split gives each check
// assertion this verdict, so a dry run refuses what the run would.
func statementText(ctx context.Context, text string, access protocol.RunStatementRequest_Access) (sqlPiece, string) {
	if len(secretvalue.References(text)) != 0 {
		return sqlPiece{}, "secret references require approved execution inputs"
	}
	piece, err := recognizePiece(text)
	if err != nil {
		return sqlPiece{}, err.Error()
	}
	analysis, err := yql.Analyze(ctx, &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{text}})
	if err != nil {
		return sqlPiece{}, "statement cannot be parsed: " + err.Error()
	}
	if len(analysis.Files[0].Statements) != 1 {
		return sqlPiece{}, "a statement request must hold exactly one statement"
	}
	if access == protocol.RunStatementRequest_READ_ONLY {
		if err := readproof.ReadOnly(text); err != nil {
			return sqlPiece{}, err.Error()
		}
	}
	return piece, ""
}

// ReadStatementRows consumes the next bounded page of a session-owned result.
func (p *Provider) ReadStatementRows(ctx context.Context, request *protocol.ReadStatementRowsRequest) (*protocol.StatementRows, error) {
	if err := provider.ValidateReadStatementRowsRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.statementResults[request.PageToken]
	if s.closed || record == nil || record.closed {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "statement result is closed or unknown")
	}
	if record.columns != request.ColumnCount {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "statement column count changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		s.failStatement(record)
		return nil, err
	}
	defer stop()
	detach := context.AfterFunc(guarded, record.cancel)
	defer detach()
	rows, more, err := record.cursor.Page(request.MaxRows)
	if err != nil || guarded.Err() != nil {
		s.failStatement(record)
		return nil, runtime.ErrorWithReason(codes.Aborted, "statement page could not be completed")
	}
	result := &protocol.StatementRows{Rows: rows}
	if more {
		result.NextPageToken = request.PageToken
	} else if err := s.releaseStatement(record); err != nil {
		return nil, err
	}
	return result, nil
}

// CloseStatement releases a result independently of the caller's cancellation.
// A retained token repeats its original cleanup result.
func (p *Provider) CloseStatement(_ context.Context, request *protocol.CloseStatementRequest) (*protocol.Empty, error) {
	if err := provider.ValidateCloseStatementRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record := s.statementResults[request.PageToken]; record != nil {
		if err := s.releaseStatement(record); err != nil {
			return nil, err
		}
	}
	return &protocol.Empty{}, nil
}

func (s *session) activeStatements() int {
	active := 0
	for _, record := range s.statementResults {
		if !record.closed {
			active++
		}
	}
	return active
}

func (s *session) failStatement(record *statementResult) {
	if tx := s.transactions[record.transaction]; tx != nil {
		tx.failed = true
	}
	// releaseStatement retains cleanup failures on the session, so a later
	// commit or close cannot report success after the original statement failed.
	_ = s.releaseStatement(record)
}

func (s *session) releaseStatement(record *statementResult) error {
	if record.closed {
		return record.closeErr
	}
	record.closed = true
	bounded, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(bounded, record.cancel)
	defer stop()
	if record.cursor != nil {
		record.closeErr = record.cursor.Close()
	}
	if record.readOnly != nil {
		err := record.readOnly.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			record.closeErr = errors.Join(record.closeErr, err)
		}
	}
	record.cancel()
	if record.conn != nil {
		record.closeErr = errors.Join(record.closeErr, closeSQLConnection(record.conn))
	}
	if record.closeErr != nil {
		record.closeErr = fmt.Errorf("statement cleanup was not confirmed")
		s.cleanupErr = errors.Join(s.cleanupErr, record.closeErr)
		if tx := s.transactions[record.transaction]; tx != nil {
			tx.failed = true
		}
	}
	return record.closeErr
}

// closeStatements releases every statement result of the session.
func (s *session) closeStatements() {
	for _, record := range s.statementResults {
		_ = s.releaseStatement(record)
	}
}

// closeTransactionStatements releases the statement results that run in
// transaction.
func (s *session) closeTransactionStatements(transaction string) {
	for _, record := range s.statementResults {
		if record.transaction == transaction {
			_ = s.releaseStatement(record)
		}
	}
}

// statementArguments binds the arguments of a statement, plain or typed, to
// the parameters $p1, $p2 and so on, in order. A typed argument binds at the
// type its caller chose; a plain one at the type its value's case names.
func statementArguments(request *protocol.RunStatementRequest) ([]any, error) {
	arguments := make([]any, 0, len(request.Arguments)+len(request.TypedArguments))
	for _, argument := range request.Arguments {
		value, err := statementrows.Argument(argument)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, sql.Named(fmt.Sprintf("p%d", len(arguments)+1), value))
	}
	for _, argument := range request.TypedArguments {
		value, err := statementrows.TypedArgument(argument)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, sql.Named(fmt.Sprintf("p%d", len(arguments)+1), value))
	}
	return arguments, nil
}
