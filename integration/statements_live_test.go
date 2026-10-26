//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func TestOperatorStatementsSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "operator_rows")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, value Utf8, PRIMARY KEY(id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+table+"` (id,value) VALUES (1u,'first'),(2u,NULL),(3u,'last')")
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.RunStatementRequest{SessionId: session.Id, RequestId: "read", Sql: "SELECT id,value FROM `" + table + "` ORDER BY id", Access: protocol.RunStatementRequest_READ_ONLY, MaxRows: 1}
	result, err := client.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(result.RowsAffected, qt.IsNil)
	c.Assert(result.Rows, qt.HasLen, 1)
	c.Assert(result.Rows[0].Values[0].GetUint64Value(), qt.Equals, uint64(1))
	token := result.NextPageToken
	c.Assert(token, qt.Not(qt.Equals), "")
	page, err := client.ReadStatementRows(c.Context(), &protocol.ReadStatementRowsRequest{SessionId: session.Id, PageToken: token, ColumnCount: 2, MaxRows: 2})
	c.Assert(err, qt.IsNil)
	c.Assert(page.Rows, qt.HasLen, 2)
	c.Assert(page.Rows[0].Values[1].GetNull(), qt.IsNotNil)
	c.Assert(page.NextPageToken, qt.Equals, "")
	for range 2 {
		_, err := client.CloseStatement(c.Context(), &protocol.CloseStatementRequest{SessionId: session.Id, PageToken: token})
		c.Assert(err, qt.IsNil)
	}
	request.Sql = "DELETE FROM `" + table + "`"
	refused, err := client.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(refused.Outcome, qt.Equals, protocol.StatementResult_REFUSED)
	assertRowCount(c, opened, table, 3)
	request.Sql = "SELECT missing_column FROM `" + table + "`"
	request.Access = protocol.RunStatementRequest_READ_WRITE
	failed, err := client.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(failed.Outcome, qt.Equals, protocol.StatementResult_FAILED)
	c.Assert(failed.Error, qt.Contains, "missing_column")
	c.Assert(failed.Error, qt.Not(qt.Contains), "127.0.0.1")
	request.Sql = "SELECT $p1 AS value"
	request.Arguments = []*protocol.DataValue{{Value: &protocol.DataValue_Int64Value{Int64Value: 17}}}
	bound, err := client.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(bound.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(bound.Rows[0].Values[0].GetInt64Value(), qt.Equals, int64(17))

	_, err = client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "operator-write", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	request.TransactionId = "operator-write"
	request.Arguments = nil
	request.Sql = "INSERT INTO `" + table + "` (id,value) VALUES (4u,'uncommitted')"
	written, err := client.RunStatement(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(written.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(written.RowsAffected, qt.IsNil)
	finished, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: request.TransactionId, Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	c.Assert(finished.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	assertRowCount(c, opened, table, 3)
}

func TestOperatorStatementTypedArgumentsSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	for _, test := range []struct {
		name  string
		value *protocol.DataValue
	}{
		{"json", &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: `{"a":1}`}}},
		{"decimal", &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: "1234", Precision: 8, Scale: 2}}}},
		{"date", &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: 20000}}},
		{"interval", &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: 2000000}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := client.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: session.Id, RequestId: test.name, Sql: "SELECT $p1 AS value", Arguments: []*protocol.DataValue{test.value}, Access: protocol.RunStatementRequest_READ_ONLY, MaxRows: 1})
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED)
			c.Assert(result.Rows, qt.HasLen, 1)
			c.Assert(proto.Equal(result.Rows[0].Values[0], test.value), qt.IsTrue, qt.Commentf("got %v, want %v", result.Rows[0].Values[0], test.value))
		})
	}
}
