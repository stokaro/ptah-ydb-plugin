//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

func executeSQL(c *qt.C, client *providerruntime.Client, session, transaction string, statements ...string) *protocol.Execution {
	c.Helper()
	request := &protocol.ExecuteRequest{SessionId: session, TransactionId: transaction, RequestId: "test"}
	for i, text := range statements {
		request.Units = append(request.Units, &protocol.Unit{ChangeId: fmt.Sprint(i), Body: &protocol.Unit_Sql{Sql: text}})
	}
	result, err := client.Execute(c.Context(), request)
	c.Assert(result, qt.IsNotNil)
	if result.Outcome == protocol.Execution_APPLIED {
		c.Assert(err, qt.IsNil)
	} else {
		var executionError *provider.ExecutionError
		c.Assert(errors.As(err, &executionError), qt.IsTrue)
		c.Assert(executionError.Outcome, qt.Equals, result.Outcome)
	}
	return result
}

func assertRowCount(c *qt.C, opened *connection.Connection, table string, expected uint64) {
	c.Helper()
	var count uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+table+"`").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, expected)
}

func TestNativeExecutionTransactionsSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "execution")
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities["transactional_ddl"], qt.IsFalse)
	result := executeSQL(c, client, session.Id, "", "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, value Utf8, PRIMARY KEY (id))")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)

	begin := &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "committed", Isolation: protocol.TransactionIsolation_SERIALIZABLE}
	call, endCall := context.WithCancel(ctx)
	handle, err := client.BeginTransaction(call, begin)
	c.Assert(err, qt.IsNil)
	endCall()
	c.Assert(handle.Isolation, qt.Equals, protocol.TransactionIsolation_SERIALIZABLE)
	insert := "UPSERT INTO `" + table + "` (id, value) VALUES (1u, \"kept\")"
	c.Assert(executeSQL(c, client, session.Id, handle.Id, insert).Outcome, qt.Equals, protocol.Execution_APPLIED)
	assertRowCount(c, opened, table, 0)
	c.Assert(executeSQL(c, client, session.Id, "", insert).Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	finish := &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: handle.Id, Action: protocol.FinishTransactionRequest_COMMIT}
	terminal, err := client.FinishTransaction(ctx, finish)
	c.Assert(err, qt.IsNil)
	c.Assert(terminal.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	assertRowCount(c, opened, table, 1)
	finish.Action = protocol.FinishTransactionRequest_ROLLBACK
	repeated, err := client.FinishTransaction(ctx, finish)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(proto.Equal(terminal, repeated), qt.IsTrue)
	_, err = client.BeginTransaction(ctx, begin)
	c.Assert(err, qt.IsNotNil)
	c.Assert(executeSQL(c, client, session.Id, handle.Id, insert).Outcome, qt.Equals, protocol.Execution_NOT_STARTED)

	for _, test := range []struct {
		id        string
		isolation protocol.TransactionIsolation
	}{
		{"rollback-default", protocol.TransactionIsolation_DEFAULT},
		{"rollback-serializable", protocol.TransactionIsolation_SERIALIZABLE},
	} {
		handle, err := client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: test.id, Isolation: test.isolation})
		c.Assert(err, qt.IsNil)
		c.Assert(handle.Isolation, qt.Equals, test.isolation)
		c.Assert(executeSQL(c, client, session.Id, handle.Id, "UPSERT INTO `"+table+"` (id) VALUES (2u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
		assertRowCount(c, opened, table, 1)
		outcome, err := client.FinishTransaction(ctx, &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: handle.Id, Action: protocol.FinishTransactionRequest_ROLLBACK})
		c.Assert(err, qt.IsNil)
		c.Assert(outcome.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
		assertRowCount(c, opened, table, 1)
	}
	fence, err := client.FinishTransaction(ctx, &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: "fenced", Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	c.Assert(fence.Outcome, qt.Equals, protocol.TransactionResult_NOT_STARTED)
	_, err = client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "fenced", Isolation: protocol.TransactionIsolation_DEFAULT})
	c.Assert(err, qt.IsNotNil)

	other, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), other.Id), qt.IsNil) })
	handle, err = client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "close-rollback", Isolation: protocol.TransactionIsolation_DEFAULT})
	c.Assert(err, qt.IsNil)
	c.Assert(executeSQL(c, client, other.Id, handle.Id, insert).Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(executeSQL(c, client, session.Id, handle.Id, "UPSERT INTO `"+table+"` (id) VALUES (3u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	assertRowCount(c, opened, table, 1)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	_, err = client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "late", Isolation: protocol.TransactionIsolation_DEFAULT})
	c.Assert(err, qt.IsNotNil)
}

func TestNativeExecutionFailureAndBatchPreflight(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "batch")
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	create := "CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))"
	request := &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "preflight", Units: []*protocol.Unit{
		{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: create}},
		{ChangeId: "native", Body: &protocol.Unit_Native{Native: &protocol.Envelope{ProviderId: "github.com/stokaro/ptah-ydb-plugin", Kind: "unsupported", SchemaVersion: 1, Encoding: "json", Payload: []byte("{}")}}},
	}}
	result, err := client.Execute(c.Context(), request)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	_, err = opened.DB.ExecContext(c.Context(), "SELECT * FROM `"+table+"`")
	c.Assert(err, qt.IsNotNil)

	result = executeSQL(c, client, session.Id, "", create, "UPSERT INTO `"+table+"` (id) VALUES (1u)", "BROKEN SQL secret-test-marker")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.AppliedCount, qt.Equals, uint32(2))
	c.Assert(result.Message, qt.Not(qt.Contains), "secret-test-marker")
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	assertRowCount(c, opened, table, 1)

	// Reusing a correlation ID is a new dispatch, not an exactly-once promise.
	first := executeSQL(c, client, session.Id, "", "INSERT INTO `"+table+"` (id) VALUES (2u)")
	c.Assert(first.Outcome, qt.Equals, protocol.Execution_APPLIED)
	second := executeSQL(c, client, session.Id, "", "INSERT INTO `"+table+"` (id) VALUES (2u)")
	c.Assert(second.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	assertRowCount(c, opened, table, 2)
}

func TestNativeTransactionRefusesDDLAndControlEscape(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	table := path.Join(database, directory, "forbidden_ddl")
	for i, statement := range []string{
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"COMMIT;",
		"ROLLBACK;",
		"SELECT 1; CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
	} {
		id := fmt.Sprint("refused-", i)
		_, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: id, Isolation: protocol.TransactionIsolation_SERIALIZABLE})
		c.Assert(err, qt.IsNil)
		result := executeSQL(c, client, session.Id, id, statement)
		c.Check(result.Outcome, qt.Not(qt.Equals), protocol.Execution_APPLIED, qt.Commentf("statement: %s", statement))
		finished, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: id, Action: protocol.FinishTransactionRequest_ROLLBACK})
		c.Assert(finished, qt.IsNotNil)
		if finished.Outcome == protocol.TransactionResult_UNKNOWN {
			c.Assert(err, qt.ErrorIs, provider.ErrOutcomeUnknown)
		} else {
			c.Assert(err, qt.IsNil)
			c.Assert(finished.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
		}
	}
	_, err = opened.DB.ExecContext(c.Context(), "SELECT * FROM `"+table+"`")
	c.Check(err, qt.IsNotNil)
	if err == nil {
		_, cleanupErr := opened.DB.ExecContext(c.Context(), "DROP TABLE `"+table+"`")
		c.Check(cleanupErr, qt.IsNil)
	}
}

func TestNativeCanceledCloseRollsBackTransaction(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "canceled_close")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	owner := backend.New()
	session, err := owner.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		c.Check(owner.Close(context.Background(), &protocol.CloseRequest{SessionId: session.Id}), qt.IsNil)
	})
	handle, err := owner.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "canceled-close", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	result, err := owner.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.Id, TransactionId: handle.Id, RequestId: "write", Units: []*protocol.Unit{{ChangeId: "write", Body: &protocol.Unit_Sql{Sql: "UPSERT INTO `" + table + "` (id) VALUES (1u)"}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(provider.CheckExecution(1, result), qt.IsNil)
	canceled, cancel := context.WithCancel(c.Context())
	cancel()
	c.Assert(owner.Close(canceled, &protocol.CloseRequest{SessionId: session.Id}), qt.IsNil)
	assertRowCount(c, opened, table, 0)
}

func TestNativeSerializableTransactionRejectsRangeConflict(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "range_conflict")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "range", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	// Both transactions observe the absent range and then write distinct rows.
	// Committing both would admit a cycle in the serialization order.
	c.Assert(executeSQL(c, client, session.Id, tx.Id, "SELECT id FROM `"+table+"` WHERE id >= 1u").Outcome, qt.Equals, protocol.Execution_APPLIED)
	_, err = opened.DB.ExecContext(c.Context(), "SELECT id FROM `"+table+"` WHERE id >= 1u; UPSERT INTO `"+table+"` (id) VALUES (2u)")
	c.Assert(err, qt.IsNil)
	written := executeSQL(c, client, session.Id, tx.Id, "UPSERT INTO `"+table+"` (id) VALUES (1u)")
	c.Assert(written.Outcome == protocol.Execution_APPLIED || written.Outcome == protocol.Execution_UNKNOWN, qt.IsTrue)
	finished, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(finished, qt.IsNotNil)
	c.Assert(finished.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(err, qt.ErrorIs, provider.ErrOutcomeUnknown)
	assertRowCount(c, opened, table, 1)
}
