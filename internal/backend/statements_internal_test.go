package backend

// White-box testing required: injected SQL driver failures and retained cursor
// handles establish cleanup and no-retry behavior that a healthy server cannot
// produce on demand. Calls use the production StatementRunner methods.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

type statementProbe struct {
	driverProbe
	queries  atomic.Int32
	closes   atomic.Int32
	rows     [][]driver.Value
	columns  []string
	types    []string
	queryErr error
	readErr  error
	args     []driver.NamedValue
}

type statementConnector struct{ probe *statementProbe }

func (c statementConnector) Driver() driver.Driver { return probeDriver{} }
func (c statementConnector) Connect(context.Context) (driver.Conn, error) {
	return &statementConnection{probeConnection: probeConnection{state: &c.probe.driverProbe}, probe: c.probe}, nil
}

type statementConnection struct {
	probeConnection
	probe *statementProbe
}

func (c *statementConnection) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	c.probe.queries.Add(1)
	c.probe.args = args
	if c.probe.queryErr != nil {
		return nil, c.probe.queryErr
	}
	return &statementDriverRows{probe: c.probe}, nil
}

type statementDriverRows struct {
	probe *statementProbe
	index int
}

func (r *statementDriverRows) Columns() []string                       { return r.probe.columns }
func (r *statementDriverRows) ColumnTypeDatabaseTypeName(i int) string { return r.probe.types[i] }
func (r *statementDriverRows) Close() error                            { r.probe.closes.Add(1); return nil }
func (r *statementDriverRows) Next(values []driver.Value) error {
	if r.index == len(r.probe.rows) {
		if r.probe.readErr != nil {
			return r.probe.readErr
		}
		return io.EOF
	}
	copy(values, r.probe.rows[r.index])
	r.index++
	return nil
}

func statementProvider(c *qt.C, probe *statementProbe) *Provider {
	c.Helper()
	db := sql.OpenDB(statementConnector{probe: probe})
	p := &Provider{sessions: map[string]*session{"session": {connection: &connection.Connection{DB: db}}}}
	c.Cleanup(func() {
		p.sessions["session"].closeStatements("", true)
		p.sessions["session"].rollbackTransactions()
		_ = db.Close()
	})
	return p
}
func statementRequest() *protocol.RunStatementRequest {
	return &protocol.RunStatementRequest{SessionId: "session", RequestId: "request", Sql: "SELECT 1 AS n", Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 1}
}

func TestStatementsPageAndClose(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n", "text"}, types: []string{"Uint64", "Optional<Utf8>"}, rows: [][]driver.Value{{uint64(1), "first"}, {uint64(2), nil}, {uint64(3), "last"}}}
	p := statementProvider(c, probe)
	request := statementRequest()
	result, err := p.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateStatementResult(request, result), qt.IsNil)
	c.Assert(result.Rows[0].Values[0].GetUint64Value(), qt.Equals, uint64(1))
	c.Assert(result.RowsAffected, qt.IsNil)
	c.Assert(result.NextPageToken, qt.Not(qt.Equals), "")
	pageRequest := &protocol.ReadStatementRowsRequest{SessionId: "session", PageToken: result.NextPageToken, MaxRows: 1, ColumnCount: 2}
	page, err := p.ReadStatementRows(c.Context(), pageRequest)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateStatementRows(pageRequest, page), qt.IsNil)
	c.Assert(page.Rows[0].Values[1].GetNull(), qt.IsNotNil)
	page, err = p.ReadStatementRows(c.Context(), pageRequest)
	c.Assert(err, qt.IsNil)
	c.Assert(page.Rows[0].Values[0].GetUint64Value(), qt.Equals, uint64(3))
	c.Assert(page.NextPageToken, qt.Equals, "")
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
	for range 2 {
		_, err := p.CloseStatement(c.Context(), &protocol.CloseStatementRequest{SessionId: "session", PageToken: result.NextPageToken})
		c.Assert(err, qt.IsNil)
	}
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
	_, err = p.ReadStatementRows(c.Context(), pageRequest)
	c.Assert(err, qt.IsNotNil)
}

func TestStatementsOwnReadOnlyTransaction(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}}}
	p := statementProvider(c, probe)
	request := statementRequest()
	request.Access = protocol.RunStatementRequest_READ_ONLY
	result, err := p.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(probe.isolation.Load(), qt.Equals, int32(sql.LevelSnapshot))
	c.Assert(probe.rollbackCalls.Load(), qt.Equals, int32(1))
	c.Assert(probe.commitCalls.Load(), qt.Equals, int32(0))
}

func TestStatementsUnknownFailureDoesNotRetry(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{queryErr: driver.ErrBadConn}
	p := statementProvider(c, probe)
	result, err := p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_UNKNOWN)
	c.Assert(result.Error, qt.Equals, "")
	c.Assert(probe.queries.Load(), qt.Equals, int32(1))
}

func TestStatementsRefuseBeforeRunning(t *testing.T) {
	for _, text := range []string{"SELECT 1; DELETE FROM t", "SELECT FileContent('secret')", "SELECT PgCall('version')", "SELECT String::Contains('a', 'b')", "DELETE FROM t"} {
		t.Run(text, func(t *testing.T) {
			c := qt.New(t)
			probe := &statementProbe{}
			p := statementProvider(c, probe)
			request := statementRequest()
			request.Sql = text
			request.Access = protocol.RunStatementRequest_READ_ONLY
			result, err := p.RunStatement(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_REFUSED)
			c.Assert(probe.queries.Load(), qt.Equals, int32(0))
		})
	}
}

func TestStatementsTransactionEndClosesRows(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}}}
	p := statementProvider(c, probe)
	_, err := p.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: "session", TransactionId: "tx", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	request := statementRequest()
	request.TransactionId = "tx"
	result, err := p.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	_, err = p.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: "session", TransactionId: "tx", Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
	c.Assert(p.sessions["session"].statementResults[result.NextPageToken].closed, qt.IsTrue)
}

func TestStatementPageFailureDiscardsPrefix(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}}, readErr: errors.New("private transport details")}
	p := statementProvider(c, probe)
	result, err := p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_UNKNOWN)
	c.Assert(result.Rows, qt.HasLen, 0)
	c.Assert(result.Error, qt.Equals, "")
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
}

func TestStatementsCloseAfterCancellation(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}}}
	p := statementProvider(c, probe)
	result, err := p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	_, err = p.CloseStatement(ctx, &protocol.CloseStatementRequest{SessionId: "session", PageToken: result.NextPageToken})
	c.Assert(err, qt.IsNil)
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
}

func TestStatementsBoundRetainedResults(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}}}
	p := statementProvider(c, probe)
	var tokens []string
	for range activeStatementLimit {
		result, err := p.RunStatement(c.Context(), statementRequest())
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
		tokens = append(tokens, result.NextPageToken)
	}
	result, err := p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_REFUSED)
	c.Assert(probe.queries.Load(), qt.Equals, int32(activeStatementLimit))
	_, err = p.CloseStatement(c.Context(), &protocol.CloseStatementRequest{SessionId: "session", PageToken: tokens[0]})
	c.Assert(err, qt.IsNil)
	result, err = p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
}

func TestStatementsRefuseOversizedMetadata(t *testing.T) {
	c := qt.New(t)
	probe := &statementProbe{columns: []string{strings.Repeat("x", 1<<20)}, types: []string{"Int64"}}
	p := statementProvider(c, probe)
	result, err := p.RunStatement(c.Context(), statementRequest())
	c.Assert(err, qt.ErrorMatches, ".*statement columns exceed the metadata byte limit")
	c.Assert(result, qt.IsNil)
	c.Assert(probe.closes.Load(), qt.Equals, int32(1))
}
