package backend

// These tests inject driver failures at the execution boundary. A live server
// cannot deterministically lose a commit reply or return ErrBadConn after dispatch.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type driverProbe struct {
	execCalls     atomic.Int32
	beginCalls    atomic.Int32
	commitCalls   atomic.Int32
	rollbackCalls atomic.Int32
	isolation     atomic.Int32
	execErr       error
	commitErr     error
	onBegin       func(context.Context) error
	onExec        func(context.Context) error
	onCommit      func(context.Context) error
}

type probeConnector struct{ state *driverProbe }

func (c probeConnector) Connect(context.Context) (driver.Conn, error) {
	return &probeConnection{state: c.state}, nil
}
func (c probeConnector) Driver() driver.Driver { return probeDriver{} }

type probeDriver struct{}

func (probeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("connector is required") }

type probeConnection struct{ state *driverProbe }

func (*probeConnection) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*probeConnection) Close() error                        { return nil }
func (*probeConnection) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *probeConnection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.state.beginCalls.Add(1)
	c.state.isolation.Store(int32(opts.Isolation))
	if c.state.onBegin != nil {
		if err := c.state.onBegin(ctx); err != nil {
			return nil, err
		}
	}
	return &probeTransaction{state: c.state, ctx: ctx}, nil
}
func (c *probeConnection) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.execCalls.Add(1)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.state.onExec != nil {
		return driver.RowsAffected(1), c.state.onExec(ctx)
	}
	return driver.RowsAffected(1), c.state.execErr
}

type probeTransaction struct {
	state *driverProbe
	ctx   context.Context
}

func (t *probeTransaction) Commit() error {
	t.state.commitCalls.Add(1)
	if t.state.onCommit != nil {
		return t.state.onCommit(t.ctx)
	}
	if t.ctx.Err() != nil {
		return t.ctx.Err()
	}
	return t.state.commitErr
}
func (t *probeTransaction) Rollback() error { t.state.rollbackCalls.Add(1); return nil }

func probeProvider(c *qt.C, probe *driverProbe) *Provider {
	c.Helper()
	db := sql.OpenDB(probeConnector{state: probe})
	s := &session{connection: &connection.Connection{DB: db}}
	p := New()
	p.sessions["session"] = s
	c.Cleanup(func() { s.rollbackTransactions(); c.Check(db.Close(), qt.IsNil) })
	return p
}
func beginRequest() *protocol.BeginTransactionRequest {
	return &protocol.BeginTransactionRequest{SessionId: "session", TransactionId: "tx", Isolation: protocol.TransactionIsolation_SERIALIZABLE}
}
func sqlRequest(transaction string) *protocol.ExecuteRequest {
	return &protocol.ExecuteRequest{SessionId: "session", TransactionId: transaction, RequestId: "write",
		Units: []*protocol.Unit{{ChangeId: "one", Body: &protocol.Unit_Sql{Sql: "write"}}}}
}
func finishRequest(action protocol.FinishTransactionRequest_Action) *protocol.FinishTransactionRequest {
	return &protocol.FinishTransactionRequest{SessionId: "session", TransactionId: "tx", Action: action}
}

func TestExecutionDoesNotRetryBadConnection(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{execErr: driver.ErrBadConn}
	p := probeProvider(c, probe)
	result, err := p.Execute(c.Context(), sqlRequest(""))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.AppliedCount, qt.Equals, uint32(0))
	c.Assert(probe.execCalls.Load(), qt.Equals, int32(1))
}

func TestTransactionOutlivesBeginRPCAndFinishesOnce(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{}
	p := probeProvider(c, probe)
	ctx, cancel := context.WithCancel(c.Context())
	handle, err := p.BeginTransaction(ctx, beginRequest())
	c.Assert(err, qt.IsNil)
	cancel()
	c.Assert(handle.Isolation, qt.Equals, protocol.TransactionIsolation_SERIALIZABLE)
	c.Assert(probe.isolation.Load(), qt.Equals, int32(sql.LevelSerializable))
	_, err = p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(status.Code(err), qt.Equals, codes.AlreadyExists)
	result, err := p.Execute(c.Context(), sqlRequest("tx"))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	outcomes := make([]*protocol.TransactionResult, 8)
	failures := make([]error, len(outcomes))
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Go(func() {
			outcomes[i], failures[i] = p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
		})
	}
	wg.Wait()
	c.Assert(failures, qt.DeepEquals, make([]error, len(outcomes)))
	for _, outcome := range outcomes {
		c.Assert(outcome.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	}
	c.Assert(probe.beginCalls.Load(), qt.Equals, int32(1))
	c.Assert(probe.commitCalls.Load(), qt.Equals, int32(1))
	c.Assert(probe.rollbackCalls.Load(), qt.Equals, int32(0))
	repeated, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(repeated, outcomes[0]), qt.IsTrue)
}

func TestUncertainCommitCannotBeRetriedOrReclassified(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{commitErr: errors.New("raw secret diagnostic")}
	p := probeProvider(c, probe)
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(err, qt.IsNil)
	unknown, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
	c.Assert(err, qt.IsNil)
	c.Assert(unknown.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(unknown.Message, qt.Not(qt.Contains), "raw secret diagnostic")
	repeated, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(unknown, repeated), qt.IsTrue)
	c.Assert(probe.commitCalls.Load(), qt.Equals, int32(1))
	c.Assert(probe.rollbackCalls.Load(), qt.Equals, int32(0))
}

func TestCanceledBeginReservesItsHandle(t *testing.T) {
	c := qt.New(t)
	started := make(chan struct{})
	probe := &driverProbe{onBegin: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}
	p := probeProvider(c, probe)
	ctx, cancel := context.WithCancel(c.Context())
	finished := make(chan error, 1)
	go func() { _, err := p.BeginTransaction(ctx, beginRequest()); finished <- err }()
	<-started
	cancel()
	c.Assert(status.Code(<-finished), qt.Equals, codes.Canceled)
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(status.Code(err), qt.Equals, codes.AlreadyExists)
	result, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(probe.beginCalls.Load(), qt.Equals, int32(1))
}

func TestCanceledFinishRetainsUncertainty(t *testing.T) {
	c := qt.New(t)
	started := make(chan struct{})
	probe := &driverProbe{onCommit: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}
	p := probeProvider(c, probe)
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	result := make(chan *protocol.TransactionResult, 1)
	failure := make(chan error, 1)
	go func() {
		value, err := p.FinishTransaction(ctx, finishRequest(protocol.FinishTransactionRequest_COMMIT))
		result <- value
		failure <- err
	}()
	<-started
	cancel()
	c.Assert((<-result).Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(<-failure, qt.IsNil)
	repeated, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
	c.Assert(err, qt.IsNil)
	c.Assert(repeated.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(probe.commitCalls.Load(), qt.Equals, int32(1))
}

func TestRollbackFencesUnusedHandleAndBoundsRetention(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{}
	p := probeProvider(c, probe)
	result, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.TransactionResult_NOT_STARTED)
	_, err = p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(status.Code(err), qt.Equals, codes.AlreadyExists)
	s := p.sessions["session"]
	for i := 1; i < transactionLimit; i++ {
		_, err := s.reserveTransaction(string(rune(i)))
		c.Assert(err, qt.IsNil)
	}
	_, err = s.reserveTransaction("overflow")
	c.Assert(status.Code(err), qt.Equals, codes.ResourceExhausted)
	c.Assert(probe.beginCalls.Load(), qt.Equals, int32(0))
}

type cancelAfterExecution struct {
	cancel context.CancelFunc
	count  int
}

func (r *cancelAfterExecution) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	r.count++
	r.cancel()
	return driver.RowsAffected(1), nil
}

func TestCancellationBetweenUnitsPreservesConfirmedPrefix(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	defer cancel()
	runner := &cancelAfterExecution{cancel: cancel}
	unit := sqlRequest("").Units[0]
	result := executeUnits(ctx, 0, nil, func(ctx context.Context, unit *protocol.Unit) (bool, error) {
		_, err := runner.ExecContext(ctx, unit.GetSql())
		return true, err
	}, []*protocol.Unit{unit, unit})
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_PARTIAL)
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	c.Assert(runner.count, qt.Equals, 1)
}

func TestNativeBatchPreflightPreventsSQLPrefix(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*protocol.ExecuteRequest)
	}{
		{"malformed operation", func(r *protocol.ExecuteRequest) {
			r.Units[1].GetNative().Payload = []byte(`{"before":null,"after":{"bad":true}}`)
		}},
		{"foreign database", func(r *protocol.ExecuteRequest) { r.Units[1].GetNative().Owner.Database = "/foreign" }},
		{"transaction", func(r *protocol.ExecuteRequest) { r.TransactionId = "tx" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			probe := &driverProbe{}
			p := probeProvider(c, probe)
			p.sessions["session"].database = "/local"
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
			desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{})
			c.Assert(err, qt.IsNil)
			change, err := schema.CaptureCoordinationChange("native", owner, nil, desired)
			c.Assert(err, qt.IsNil)
			operation, err := schema.CoordinationOperationFromChange("native", change.Value)
			c.Assert(err, qt.IsNil)
			request := sqlRequest("")
			request.Units = append(request.Units, &protocol.Unit{ChangeId: "native", Body: &protocol.Unit_Native{Native: operation.Value}})
			test.modify(request)
			result, err := p.Execute(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
			c.Assert(result.AppliedCount, qt.Equals, uint32(0))
			c.Assert(probe.execCalls.Load(), qt.Equals, int32(0))
		})
	}
}

func TestExecutionDistinguishesRefusalFromUnknownDispatch(t *testing.T) {
	for _, test := range []struct {
		name       string
		prefix     int
		dispatched bool
		outcome    protocol.Execution_Outcome
	}{
		{"preflight", 0, false, protocol.Execution_NOT_STARTED}, {"prefix refused", 1, false, protocol.Execution_PARTIAL}, {"lost reply", 0, true, protocol.Execution_UNKNOWN}, {"prefix lost reply", 1, true, protocol.Execution_UNKNOWN},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			calls := 0
			result := executeUnits(t.Context(), 0, nil, func(context.Context, *protocol.Unit) (bool, error) {
				calls++
				if calls > test.prefix {
					return test.dispatched, errors.New("secret-marker")
				}
				return true, nil
			}, []*protocol.Unit{{}, {}, {}})
			c.Assert(result.Outcome, qt.Equals, test.outcome)
			c.Assert(result.AppliedCount, qt.Equals, uint32(test.prefix))
			c.Assert(calls, qt.Equals, test.prefix+1)
			c.Assert(result.Message, qt.Not(qt.Contains), "secret-marker")
		})
	}
}

// A statement timeout reaches the provider as the deadline of the Execute call.
// A unit the deadline stops has been dispatched and YDB may keep running it, so
// the outcome is unknown, no later unit runs, and the unit is not sent again.
func TestExecutionStoppedByADeadlineIsUnknown(t *testing.T) {
	c := qt.New(t)
	started := make(chan struct{})
	probe := &driverProbe{onExec: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}
	p := probeProvider(c, probe)
	ctx, cancel := context.WithTimeout(c.Context(), 100*time.Millisecond)
	defer cancel()
	request := sqlRequest("")
	request.Units = append(request.Units, request.Units[0], request.Units[0])

	result, err := p.Execute(ctx, request)

	<-started
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.AppliedCount, qt.Equals, uint32(0))
	c.Assert(result.DiagnosticCode, qt.Equals, "ydb.execution-unconfirmed")
	c.Assert(probe.execCalls.Load(), qt.Equals, int32(1))
}

// A deadline that expires after earlier units were confirmed keeps them counted.
func TestExecutionStoppedByADeadlineKeepsTheConfirmedPrefix(t *testing.T) {
	c := qt.New(t)
	var calls atomic.Int32
	probe := &driverProbe{onExec: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	p := probeProvider(c, probe)
	ctx, cancel := context.WithTimeout(c.Context(), 200*time.Millisecond)
	defer cancel()
	request := sqlRequest("")
	request.Units = append(request.Units, request.Units[0])

	result, err := p.Execute(ctx, request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	c.Assert(probe.execCalls.Load(), qt.Equals, int32(2))
}

func TestSessionReportsTheStatementTimeoutCapability(t *testing.T) {
	c := qt.New(t)
	c.Assert(sessionCapabilities("26.2.1.14"), qt.DeepEquals, map[string]bool{
		"transactional_ddl": false, "secrets": true, "coordination_nodes": true, "migration_statement_timeout": true, "tiered_ttl": false,
		"role_management": true, "role_membership": true, "group_principals": true, "database_grants": true, "relative_grant_paths": true,
		"topics": true, "topic_consumer_availability_period": true,
		"changefeeds": true, "changefeed_user_sids": true, "changefeed_schema_changes": true, "changefeed_topic_auto_partitioning": true,
		"external_data_sources": false, "external_object_replace": false, "external_data_source_secret_paths": true,
		"resource_pools": false, "streaming_queries": false,
		"wide_date_time_types": true, "parameterized_decimal": true, "alter_column_default": true, "add_column_with_default": true, "small_integer_defaults": true, "document_type_defaults": true,
	})
	// 25.1 takes none of the later changefeed options, and 25.1 to 25.3 take
	// no availability period on a consumer and no secret path on a data
	// source.
	c.Assert(sessionCapabilities("25.1.4.7"), qt.DeepEquals, map[string]bool{
		"transactional_ddl": false, "secrets": false, "coordination_nodes": true, "migration_statement_timeout": true, "tiered_ttl": false,
		"role_management": true, "role_membership": true, "group_principals": true, "database_grants": true, "relative_grant_paths": false,
		"topics": true, "topic_consumer_availability_period": false,
		"changefeeds": true, "changefeed_user_sids": false, "changefeed_schema_changes": false, "changefeed_topic_auto_partitioning": false,
		"external_data_sources": false, "external_object_replace": false, "external_data_source_secret_paths": false,
		"resource_pools": false, "streaming_queries": false,
		"wide_date_time_types": false, "parameterized_decimal": false, "alter_column_default": false, "add_column_with_default": false, "small_integer_defaults": false, "document_type_defaults": false,
	})
	// Each option arrives on the line the host's presets name.
	for _, test := range []struct {
		version string
		key     string
		want    bool
	}{
		{"25.2.1", "changefeed_topic_auto_partitioning", true},
		{"25.4.1", "add_column_with_default", false},
		{"26.1.1", "add_column_with_default", true},
		{"25.2.1", "changefeed_schema_changes", false},
		{"25.3.1", "changefeed_schema_changes", true},
		{"25.4.1", "changefeed_user_sids", false},
		{"26.1.1", "changefeed_user_sids", true},
		{"25.3.1", "external_data_source_secret_paths", false},
		{"25.4.1", "external_data_source_secret_paths", true},
	} {
		c.Assert(sessionCapabilities(test.version)[test.key], qt.Equals, test.want, qt.Commentf("%s on %s", test.key, test.version))
	}
}

// blockUntilDone is a unit that runs until its context ends.
func blockUntilDone(ctx context.Context, unit *protocol.Unit) (bool, error) {
	if unit.GetChangeId() == "quick" {
		return true, nil
	}
	<-ctx.Done()
	return true, ctx.Err()
}

// TestStatementTimeoutAnswersForTheStoppedUnit pins what a unit the statement
// timeout stops reports: what the session proves about it decides the
// outcome, and the reply says the timeout stopped it.
func TestStatementTimeoutAnswersForTheStoppedUnit(t *testing.T) {
	for _, test := range []struct {
		name    string
		units   []*protocol.Unit
		stopped stopOutcome
		outcome protocol.Execution_Outcome
		applied uint32
		flagged bool
	}{
		{"nothing applied", []*protocol.Unit{{ChangeId: "slow"}}, stopNotApplied, protocol.Execution_NOT_APPLIED, 0, true},
		{"after a prefix", []*protocol.Unit{{ChangeId: "quick"}, {ChangeId: "slow"}}, stopNotApplied, protocol.Execution_PARTIAL, 1, true},
		{"unknown", []*protocol.Unit{{ChangeId: "quick"}, {ChangeId: "slow"}}, stopUnknown, protocol.Execution_UNKNOWN, 1, true},
		{"applied after all", []*protocol.Unit{{ChangeId: "slow"}, {ChangeId: "quick"}}, stopApplied, protocol.Execution_APPLIED, 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			var asked []string
			stopped := func(unit *protocol.Unit) stopOutcome {
				asked = append(asked, unit.GetChangeId())
				return test.stopped
			}

			result := executeUnits(t.Context(), 20*time.Millisecond, stopped, blockUntilDone, test.units)

			c.Assert(result.Outcome, qt.Equals, test.outcome)
			c.Assert(result.AppliedCount, qt.Equals, test.applied)
			c.Assert(result.StatementTimedOut, qt.Equals, test.flagged)
			c.Assert(asked, qt.DeepEquals, []string{"slow"})
		})
	}
}

// TestCanceledCallIsNotATimeout pins that a call the caller canceled is not
// reported as stopped by the statement timeout.
func TestCanceledCallIsNotATimeout(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	stopped := func(*protocol.Unit) stopOutcome { return stopNotApplied }

	result := executeUnits(ctx, time.Hour, stopped, blockUntilDone, []*protocol.Unit{{ChangeId: "slow"}})

	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.StatementTimedOut, qt.IsFalse)
}

func TestDefaultAdditionRefusesTheWholeBatchBeforeDispatch(t *testing.T) {
	for _, test := range []struct{ name, transaction, code string }{
		{name: "capability absent", code: "ydb.column-addition-unsupported"},
		{name: "transaction", transaction: "tx", code: "ydb.native-transaction-forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			probe := &driverProbe{}
			service := probeProvider(c, probe)
			kind := &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}
			value, err := tablecreate.CaptureInventory("add", &protocol.Reference{Engine: "ydb", Kind: "table", Name: "records"}, &tablecreate.ColumnInventory{Add: &Ydb_Table.ColumnMeta{Name: "label", Type: kind, NotNull: new(true), DefaultValue: &Ydb_Table.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: kind, Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: ""}}}}}})
			c.Assert(err, qt.IsNil)
			request := sqlRequest(test.transaction)
			request.Units = append(request.Units, &protocol.Unit{ChangeId: "add", Body: &protocol.Unit_Native{Native: value.Value}})
			result, err := service.Execute(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
			c.Assert(result.DiagnosticCode, qt.Equals, test.code)
			c.Assert(result.AppliedCount, qt.Equals, uint32(0))
			c.Assert(probe.execCalls.Load(), qt.Equals, int32(0))
		})
	}
}
