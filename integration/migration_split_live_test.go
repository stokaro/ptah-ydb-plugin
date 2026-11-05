//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// These tests run the units SplitMigration returns as the host runs them under
// units.migration-split (ADR 0054): a WITH_CHECKPOINT unit in a serializable
// transaction of its own together with the history write that records it, and
// an OUTSIDE unit with no transaction.

// splitSession opens a session on the test database for a host that lists
// hostFeatures.
func splitSession(c *qt.C, hostFeatures ...string) (*providerruntime.Client, string) {
	c.Helper()
	client := startProviderFor(c, hostFeatures...)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return client, session.Id
}

// splitBody splits body as the host does before a run.
func splitBody(c *qt.C, client *providerruntime.Client, body string, assertions ...string) *protocol.MigrationSplit {
	c.Helper()
	split, err := client.SplitMigration(c.Context(), &protocol.SplitMigrationRequest{Engine: "ydb", Body: body, Assertions: assertions})
	c.Assert(err, qt.IsNil)
	c.Assert(split.Refusal, qt.IsNil)
	return split
}

func beginSerializable(c *qt.C, client *providerruntime.Client, session, id string) {
	c.Helper()
	_, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session, TransactionId: id, Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
}

func finish(c *qt.C, client *providerruntime.Client, session, id string, action protocol.FinishTransactionRequest_Action) (*protocol.TransactionResult, error) {
	c.Helper()
	return client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session, TransactionId: id, Action: action})
}

// checkpoint records progress over the migration's revision row, in the
// transaction the scope names, if any.
func checkpoint(c *qt.C, client *providerruntime.Client, scope *protocol.HistoryScope, before *protocol.HistoryRevision, applied uint64) *protocol.HistoryRevision {
	c.Helper()
	after := &protocol.HistoryRevision{Version: "1", Description: "split", State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_UP, Applied: applied, Total: 4}
	result, err := client.MutateHistory(c.Context(), &protocol.MutateHistoryRequest{Scope: scope, RequestId: "checkpoint", Expected: before, Replacement: after})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_APPLIED)
	return after
}

// runOutside runs an OUTSIDE unit with no transaction and checkpoints it.
func runOutside(c *qt.C, client *providerruntime.Client, scope *protocol.HistoryScope, unit *protocol.MigrationUnit, before *protocol.HistoryRevision, applied uint64) *protocol.HistoryRevision {
	c.Helper()
	c.Assert(unit.Placement, qt.Equals, protocol.MigrationUnit_OUTSIDE)
	c.Assert(executeSQL(c, client, scope.SessionId, "", unit.Sql).Outcome, qt.Equals, protocol.Execution_APPLIED)
	return checkpoint(c, client, scope, before, applied)
}

// runWithCheckpoint runs a WITH_CHECKPOINT unit and its checkpoint in one
// serializable transaction and commits them.
func runWithCheckpoint(c *qt.C, client *providerruntime.Client, scope *protocol.HistoryScope, unit *protocol.MigrationUnit, before *protocol.HistoryRevision, applied uint64) *protocol.HistoryRevision {
	c.Helper()
	c.Assert(unit.Placement, qt.Equals, protocol.MigrationUnit_WITH_CHECKPOINT)
	id := "unit-" + unit.Source
	beginSerializable(c, client, scope.SessionId, id)
	c.Assert(executeSQL(c, client, scope.SessionId, id, unit.Sql).Outcome, qt.Equals, protocol.Execution_APPLIED)
	inTransaction := &protocol.HistoryScope{SessionId: scope.SessionId, TransactionId: id, Schema: scope.Schema, Table: scope.Table, Format: scope.Format}
	after := checkpoint(c, client, inTransaction, before, applied)
	committed, err := finish(c, client, scope.SessionId, id, protocol.FinishTransactionRequest_COMMIT)
	c.Assert(err, qt.IsNil)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	return after
}

func dropAtCleanup(c *qt.C, opened *connection.Connection, table string) {
	c.Helper()
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
}

// seededTable creates a table of two rows, id 1 and 2, both "a".
func seededTable(c *qt.C, opened *connection.Connection, table string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, opened.DB, "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))"), qt.IsNil)
	dropAtCleanup(c, opened, table)
	_, err := opened.DB.ExecContext(ctx, "UPSERT INTO `"+table+"` (id, v) VALUES (1u, \"a\"u), (2u, \"a\"u)")
	c.Assert(err, qt.IsNil)
}

func rowValue(c *qt.C, opened *connection.Connection, table string, id uint64) string {
	c.Helper()
	var value string
	c.Assert(opened.DB.QueryRowContext(c.Context(), "DECLARE $id AS Uint64; SELECT v FROM `"+table+"` WHERE id = $id", sql.Named("id", id)).Scan(&value), qt.IsNil)
	return value
}

// TestMigrationSplitRunsItsUnitsOnTheServer runs a body as the host would: a
// scheme statement outside a transaction, the data query that reads a named
// expression the text defined above it in a transaction with its checkpoint,
// and a scheme block and a BATCH statement outside one. Each unit's
// checkpoint is recorded, and the rows and tables are read back.
func TestMigrationSplitRunsItsUnitsOnTheServer(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "rows")
	made := path.Join(database, directory, "made")
	client, session := splitSession(c, provider.MigrationSplitFeature, provider.StatementTimeoutFeature)
	body := "CREATE TABLE `" + table + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));\n" +
		"$v = \"a\"u;\n" +
		"UPSERT INTO `" + table + "` (id, v) VALUES (1u, $v);\n" +
		"UPSERT INTO `" + table + "` (id, v) VALUES (2u, $v || \"b\"u);\n" +
		"DO BEGIN CREATE TABLE `" + made + "` (id Uint64 NOT NULL, PRIMARY KEY (id)); END DO;\n" +
		"BATCH UPDATE `" + table + "` SET v = $v || \"c\"u WHERE id > 1u;\n"

	split := splitBody(c, client, body, "SELECT COUNT(*) = 2 FROM `"+table+"`", "DELETE FROM `"+table+"`")

	c.Assert(split.Units, qt.HasLen, 4)
	c.Assert(split.Units[1], qt.CmpEquals(protocmp.Transform()), &protocol.MigrationUnit{
		Sql:       "$v = \"a\"u;\nUPSERT INTO `" + table + "` (id, v) VALUES (1u, $v);\nUPSERT INTO `" + table + "` (id, v) VALUES (2u, $v || \"b\"u)",
		Source:    "$v = \"a\"u;\nUPSERT INTO `" + table + "` (id, v) VALUES (1u, $v);\nUPSERT INTO `" + table + "` (id, v) VALUES (2u, $v || \"b\"u);",
		Placement: protocol.MigrationUnit_WITH_CHECKPOINT,
	})
	c.Assert(split.Assertions[0].ReadOnly, qt.IsTrue)
	c.Assert(split.Assertions[1].Reason, qt.Not(qt.Equals), "")

	scope := &protocol.HistoryScope{SessionId: session, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	prepared, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Outcome, qt.Equals, protocol.HistoryResult_APPLIED)
	dropAtCleanup(c, opened, path.Join(database, directory, "revisions"))
	started := checkpoint(c, client, scope, nil, 0)

	created := runOutside(c, client, scope, split.Units[0], started, 1)
	dropAtCleanup(c, opened, table)
	written := runWithCheckpoint(c, client, scope, split.Units[1], created, 2)
	block := runOutside(c, client, scope, split.Units[2], written, 3)
	dropAtCleanup(c, opened, made)
	runOutside(c, client, scope, split.Units[3], block, 4)

	c.Assert(rowValue(c, opened, table, 1), qt.Equals, "a")
	c.Assert(rowValue(c, opened, table, 2), qt.Equals, "ac")
	assertRowCount(c, opened, made, 0)
	recorded := historyRead(c, client, scope)
	c.Assert(recorded.Revisions, qt.HasLen, 1)
	c.Assert(recorded.Revisions[0].Applied, qt.Equals, uint64(4))
}

// TestMigrationSplitUnitRejected_FailurePath runs units YDB refuses before
// applying anything: a data unit that inserts a key that exists, in the
// transaction that also holds its checkpoint; an OUTSIDE unit that drops a
// table that does not exist; and a scheme unit run where its placement says
// it cannot run, inside a transaction. Each is NOT_APPLIED with the server's
// reason, and the transaction it ran in is rolled back with its checkpoint.
func TestMigrationSplitUnitRejected_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "rows")
	seededTable(c, opened, table)
	client, session := splitSession(c, provider.MigrationSplitFeature)
	scope := &protocol.HistoryScope{SessionId: session, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	_, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	dropAtCleanup(c, opened, path.Join(database, directory, "revisions"))
	split := splitBody(c, client, "INSERT INTO `"+table+"` (id, v) VALUES (3u, \"new\"u), (1u, \"x\"u);\n"+
		"DROP TABLE `"+path.Join(database, directory, "missing")+"`;\n"+
		"CREATE TABLE `"+path.Join(database, directory, "late")+"` (id Uint64 NOT NULL, PRIMARY KEY (id));\n")
	c.Assert(split.Units, qt.HasLen, 3)

	beginSerializable(c, client, session, "insert")
	inTransaction := &protocol.HistoryScope{SessionId: session, TransactionId: "insert", Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	checkpoint(c, client, inTransaction, nil, 1)
	inserted := executeSQL(c, client, session, "insert", split.Units[0].Sql)
	c.Assert(inserted.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(inserted.DiagnosticCode, qt.Equals, "ydb.statement-rejected")
	c.Assert(inserted.Message, qt.Contains, "Conflict with existing key")
	c.Assert(inserted.TransactionAborted, qt.IsFalse)
	rolledBack, err := finish(c, client, session, "insert", protocol.FinishTransactionRequest_ROLLBACK)
	c.Assert(err, qt.IsNil)
	c.Assert(rolledBack.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	assertRowCount(c, opened, table, 2)
	c.Assert(historyRead(c, client, scope).Revisions, qt.HasLen, 0)

	dropped := executeSQL(c, client, session, "", split.Units[1].Sql)
	c.Assert(split.Units[1].Placement, qt.Equals, protocol.MigrationUnit_OUTSIDE)
	c.Assert(dropped.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(dropped.Message, qt.Contains, "Cannot find table")

	beginSerializable(c, client, session, "scheme")
	misplaced := executeSQL(c, client, session, "scheme", split.Units[2].Sql)
	c.Assert(split.Units[2].Placement, qt.Equals, protocol.MigrationUnit_OUTSIDE)
	c.Assert(misplaced.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(misplaced.Message, qt.Contains, "Scheme operations cannot be executed inside transaction")
	committed, err := finish(c, client, session, "scheme", protocol.FinishTransactionRequest_COMMIT)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(committed.Aborted, qt.IsFalse)
	_, err = opened.DB.ExecContext(c.Context(), "SELECT * FROM `"+path.Join(database, directory, "late")+"`")
	c.Assert(err, qt.IsNotNil)
}

// conflictingWrite commits a write of key id from a transaction of another
// session, which invalidates the locks of a transaction that read the key.
func conflictingWrite(c *qt.C, client *providerruntime.Client, session, table string, id string) {
	c.Helper()
	beginSerializable(c, client, session, "conflict-"+id)
	c.Assert(executeSQL(c, client, session, "conflict-"+id, "UPSERT INTO `"+table+"` (id, v) VALUES ("+id+"u, \"other\"u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
	committed, err := finish(c, client, session, "conflict-"+id, protocol.FinishTransactionRequest_COMMIT)
	c.Assert(err, qt.IsNil)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
}

// TestMigrationSplitConflictAbortSetsTheFlags runs two transactions that write
// the same key under serializable isolation. The one that read the key and
// commits second is aborted: at its commit, which is ROLLED_BACK with aborted,
// and at a write after a read that saw the conflict, which is NOT_APPLIED with
// transaction_aborted. Neither applies anything.
func TestMigrationSplitConflictAbortSetsTheFlags(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "rows")
	seededTable(c, opened, table)
	client, session := splitSession(c, provider.MigrationSplitFeature)
	writer, other := splitSession(c, provider.MigrationSplitFeature)
	unit := splitBody(c, client, "UPSERT INTO `"+table+"` SELECT id, v || \"x\"u AS v FROM `"+table+"` WHERE id = 1u;").Units[0]
	c.Assert(unit.Placement, qt.Equals, protocol.MigrationUnit_WITH_CHECKPOINT)

	beginSerializable(c, client, session, "commit")
	c.Assert(executeSQL(c, client, session, "commit", unit.Sql).Outcome, qt.Equals, protocol.Execution_APPLIED)
	conflictingWrite(c, writer, other, table, "1")
	aborted, err := finish(c, client, session, "commit", protocol.FinishTransactionRequest_COMMIT)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(aborted.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(aborted.Aborted, qt.IsTrue)
	c.Assert(aborted.Message, qt.Contains, "Transaction locks invalidated")
	c.Assert(rowValue(c, opened, table, 1), qt.Equals, "other")

	beginSerializable(c, client, session, "execute")
	read := "SELECT v FROM `" + table + "` WHERE id = 2u"
	c.Assert(executeSQL(c, client, session, "execute", read).Outcome, qt.Equals, protocol.Execution_APPLIED)
	conflictingWrite(c, writer, other, table, "2")
	c.Assert(executeSQL(c, client, session, "execute", read).Outcome, qt.Equals, protocol.Execution_APPLIED)
	write := executeSQL(c, client, session, "execute", "UPSERT INTO `"+table+"` (id, v) VALUES (2u, \"mine\"u)")
	c.Assert(write.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(write.TransactionAborted, qt.IsTrue)
	c.Assert(write.DiagnosticCode, qt.Equals, "ydb.transaction-aborted")
	rolledBack, err := finish(c, client, session, "execute", protocol.FinishTransactionRequest_ROLLBACK)
	c.Assert(err, qt.IsNil)
	c.Assert(rolledBack.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(rowValue(c, opened, table, 2), qt.Equals, "other")
}

// TestConflictAbortWithoutTheMigrationSplitSetsNoFlag is the control: a host
// that did not list units.migration-split gets the same outcomes without the
// flags, which it could not read.
func TestConflictAbortWithoutTheMigrationSplitSetsNoFlag(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "rows")
	seededTable(c, opened, table)
	client, session := splitSession(c)
	writer, other := splitSession(c)

	beginSerializable(c, client, session, "commit")
	c.Assert(executeSQL(c, client, session, "commit", "UPSERT INTO `"+table+"` SELECT id, v || \"x\"u AS v FROM `"+table+"` WHERE id = 1u").Outcome, qt.Equals, protocol.Execution_APPLIED)
	conflictingWrite(c, writer, other, table, "1")
	aborted, err := finish(c, client, session, "commit", protocol.FinishTransactionRequest_COMMIT)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(aborted.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(aborted.Aborted, qt.IsFalse)
	c.Assert(rowValue(c, opened, table, 1), qt.Equals, "other")

	beginSerializable(c, client, session, "execute")
	read := "SELECT v FROM `" + table + "` WHERE id = 2u"
	c.Assert(executeSQL(c, client, session, "execute", read).Outcome, qt.Equals, protocol.Execution_APPLIED)
	conflictingWrite(c, writer, other, table, "2")
	c.Assert(executeSQL(c, client, session, "execute", read).Outcome, qt.Equals, protocol.Execution_APPLIED)
	write := executeSQL(c, client, session, "execute", "UPSERT INTO `"+table+"` (id, v) VALUES (2u, \"mine\"u)")
	c.Assert(write.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(write.TransactionAborted, qt.IsFalse)
	c.Assert(rowValue(c, opened, table, 2), qt.Equals, "other")
}
