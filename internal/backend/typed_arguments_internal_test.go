package backend

// White-box testing required: the probe connection records the parameters the
// provider hands database/sql, which a live server consumes without showing,
// and a refused statement must reach no connection at all.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// checkingConnection accepts every parameter as it is, as the YDB driver
// accepts a types.Value, so the probe records what the provider bound.
type checkingConnection struct{ *statementConnection }

func (checkingConnection) CheckNamedValue(*driver.NamedValue) error { return nil }

type checkingConnector struct{ probe *statementProbe }

func (c checkingConnector) Driver() driver.Driver { return probeDriver{} }
func (c checkingConnector) Connect(context.Context) (driver.Conn, error) {
	return checkingConnection{&statementConnection{probeConnection: probeConnection{state: &c.probe.driverProbe}, probe: c.probe}}, nil
}

func checkingProvider(c *qt.C, probe *statementProbe) *Provider {
	c.Helper()
	db := sql.OpenDB(checkingConnector{probe: probe})
	p := &Provider{sessions: map[string]*session{"session": {connection: &connection.Connection{DB: db}}}}
	c.Cleanup(func() {
		p.sessions["session"].closeStatements()
		_ = db.Close()
	})
	return p
}

func typedRequest(text string, arguments ...*protocol.StatementArgument) *protocol.RunStatementRequest {
	request := statementRequest()
	request.Sql, request.TypedArguments = text, arguments
	return request
}

// Typed arguments bind to $p1, $p2 and so on in order, each at the YDB type
// its caller bound it at.
func TestStatementsBindTypedArguments(t *testing.T) {
	c := testcheck.New(t)
	probe := &statementProbe{columns: []string{"n"}, types: []string{"Int64"}, rows: [][]driver.Value{{int64(1)}}}
	p := checkingProvider(c, probe)
	request := typedRequest("SELECT $p1, $p2, $p3 AS n",
		&protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: protocol.ArgumentType_INT32}, Value: &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: -5}}},
		&protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: protocol.ArgumentType_STRING, Optional: true}, Value: &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}},
		&protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: protocol.ArgumentType_UINT64, List: true}, Elements: []*protocol.DataValue{
			{Value: &protocol.DataValue_Uint64Value{Uint64Value: 1}}}})
	result, err := p.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
	bound := make([][2]string, 0, len(probe.args))
	for _, argument := range probe.args {
		value, ok := argument.Value.(types.Value)
		c.Assert(ok, qt.IsTrue, qt.Commentf("%s holds %T", argument.Name, argument.Value))
		bound = append(bound, [2]string{argument.Name, value.Type().Yql()})
	}
	c.Assert(bound, qt.DeepEquals, [][2]string{{"p1", "Int32"}, {"p2", "Optional<Utf8>"}, {"p3", "List<Uint64>"}})
}

// A typed argument YDB cannot hold, and any argument of a comment or
// coordination statement, refuses the statement before it reaches a
// connection.
func TestStatementsRefuseTypedArgumentsBeforeRunning(t *testing.T) {
	later := &protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: protocol.ArgumentType_TIMESTAMP}, Value: &protocol.DataValue{
		Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 4291747200}}}}
	small := &protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: protocol.ArgumentType_INT32}, Value: &protocol.DataValue{
		Value: &protocol.DataValue_Int64Value{Int64Value: 1}}}
	for _, test := range []struct {
		name    string
		request *protocol.RunStatementRequest
		want    string
	}{
		{name: "a timestamp outside its range", request: typedRequest("SELECT $p1 AS n", later), want: "YDB timestamp is outside its range"},
		{name: "a comment statement", request: typedRequest("COMMENT ON TABLE `t` IS 'x'", small),
			want: "a comment or coordination statement runs without a transaction or bound arguments"},
		{name: "a coordination statement", request: typedRequest("ALTER COORDINATION NODE `n` SET (read_consistency_mode = 'strict')", small),
			want: "a comment or coordination statement runs without a transaction or bound arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			probe := &statementProbe{}
			p := checkingProvider(c, probe)
			result, err := p.RunStatement(c.Context(), test.request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_REFUSED)
			c.Assert(result.Refusal, qt.Equals, test.want)
			c.Assert(probe.queries.Load(), qt.Equals, int32(0))
		})
	}
}
