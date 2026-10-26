package backend

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/readproof"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/internal/statementrows"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	refuse := func(reason string) (*protocol.StatementResult, error) {
		return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_REFUSED, Refusal: rowdata.OneLine(reason)}, nil
	}
	if len(secretvalue.References(request.Sql)) != 0 {
		return refuse("secret references require approved execution inputs")
	}
	piece, err := recognizePiece(request.Sql)
	if err != nil {
		return refuse(err.Error())
	}
	if piece.service() && (request.TransactionId != "" || len(request.Arguments) != 0) {
		return refuse("a comment or coordination statement runs without a transaction or bound arguments")
	}
	analysis, err := yql.Analyze(ctx, &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{request.Sql}})
	if err != nil {
		return refuse("statement cannot be parsed: " + err.Error())
	}
	if len(analysis.Files[0].Statements) != 1 {
		return refuse("a statement request must hold exactly one statement")
	}
	arguments := make([]any, len(request.Arguments))
	for i, argument := range request.Arguments {
		value, err := statementrows.Argument(argument)
		if err != nil {
			return refuse(err.Error())
		}
		arguments[i] = sql.Named(fmt.Sprintf("p%d", i+1), value)
	}
	if request.Access == protocol.RunStatementRequest_READ_ONLY {
		if err := readproof.ReadOnly(request.Sql); err != nil {
			return refuse(err.Error())
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
	if s.realm {
		if err := yql.ConfineToRealm(request.Sql); err != nil {
			return refuse(err.Error())
		}
	}
	if len(s.statementResults) >= statementLimit || s.activeStatements() >= activeStatementLimit {
		return refuse("statement result limit reached; close results or open another session")
	}
	if request.TransactionId == "" && s.active != "" {
		return refuse("a transaction is active on this session")
	}
	if request.TransactionId != "" {
		tx := s.transactions[request.TransactionId]
		if tx == nil || tx.tx == nil || tx.failed || s.active != request.TransactionId {
			return refuse("statement transaction is inactive")
		}
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
		dispatched, err := s.runPiece(ctx, nil, piece)
		if err != nil {
			if !dispatched {
				return refuse(err.Error())
			}
			return statementFailure(request.RequestId, err, request.Sql), nil
		}
		if ctx.Err() != nil || s.verifyLocks(ctx) != nil {
			return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_UNKNOWN}, nil
		}
		return &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_SUCCEEDED}, nil
	}
	// The RPC cancellation controls work while this call is active. A returned
	// cursor owns its lifetime until a later page, close, or transaction end.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	detach := context.AfterFunc(ctx, cancel)
	defer detach()
	record := &statementResult{transaction: request.TransactionId, cancel: cancel}
	var runner rowReader
	if request.TransactionId != "" {
		runner = s.transactions[request.TransactionId].tx
	} else {
		record.conn, err = s.connection.DB.Conn(ctx)
		if err != nil {
			cancel()
			return refuse("statement connection is unavailable")
		}
		runner = record.conn
		if request.Access == protocol.RunStatementRequest_READ_ONLY {
			record.readOnly, err = record.conn.BeginTx(lifetime, &sql.TxOptions{Isolation: sql.LevelSnapshot, ReadOnly: true})
			if err != nil {
				_ = s.releaseStatement(record)
				return refuse("cannot open a snapshot read-only transaction")
			}
			runner = record.readOnly
		}
	}
	rows, err := runner.QueryContext(lifetime, request.Sql, arguments...)
	if err != nil {
		s.failStatement(record)
		return statementFailure(request.RequestId, err, request.Sql), nil
	}
	record.cursor, err = statementrows.New(rows)
	if err != nil {
		_ = rows.Close()
		s.failStatement(record)
		return nil, status.Error(codes.FailedPrecondition, rowdata.OneLine(err.Error()))
	}
	result := &protocol.StatementResult{RequestId: request.RequestId, Outcome: protocol.StatementResult_SUCCEEDED, Columns: record.cursor.Columns()}
	record.columns = uint32(len(result.Columns))
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
		if s.statementResults == nil {
			s.statementResults = make(map[string]*statementResult)
		}
		result.NextPageToken = rand.Text()
		s.statementResults[result.NextPageToken] = record
	} else if err := s.releaseStatement(record); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadStatementRows consumes the next bounded page of a session-owned result.
func (p *Provider) ReadStatementRows(ctx context.Context, request *protocol.ReadStatementRowsRequest) (*protocol.StatementRows, error) {
	if err := provider.ValidateReadStatementRowsRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, err := p.lookup(request.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.statementResults[request.PageToken]
	if s.closed || record == nil || record.closed {
		return nil, status.Error(codes.FailedPrecondition, "statement result is closed or unknown")
	}
	if record.columns != request.ColumnCount {
		return nil, status.Error(codes.InvalidArgument, "statement column count changed")
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
		return nil, status.Error(codes.Aborted, "statement page could not be completed")
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
		return nil, status.Error(codes.InvalidArgument, err.Error())
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

func (s *session) closeStatements(transaction string, all bool) {
	for _, record := range s.statementResults {
		if all || record.transaction == transaction {
			_ = s.releaseStatement(record)
		}
	}
}
