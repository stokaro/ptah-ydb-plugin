//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"sync"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

func historySession(c *qt.C, binary, digest string) (*providerruntime.Client, string, func()) {
	c.Helper()
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	var once sync.Once
	closeSession := func() { once.Do(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) }) }
	c.Cleanup(closeSession)
	return client, session.Id, closeSession
}
func historyRead(c *qt.C, client *providerruntime.Client, scope *protocol.HistoryScope) *protocol.HistorySnapshot {
	c.Helper()
	request := &protocol.ReadHistoryRequest{Scope: scope, MaxRevisions: 100}
	result, err := client.ReadHistory(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateHistorySnapshot(request, result), qt.IsNil)
	return result
}
func historyChange(c *qt.C, client *providerruntime.Client, scope *protocol.HistoryScope, before, after *protocol.HistoryRevision, want protocol.HistoryResult_Outcome) {
	c.Helper()
	request := &protocol.MutateHistoryRequest{Scope: scope, RequestId: "cas", Expected: before, Replacement: after}
	result, err := client.MutateHistory(c.Context(), request)
	c.Assert(result, qt.IsNotNil)
	c.Assert(provider.ValidateHistoryResult(scope, "cas", result), qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, want)
	switch want {
	case protocol.HistoryResult_APPLIED:
		c.Assert(err, qt.IsNil)
	case protocol.HistoryResult_UNKNOWN:
		c.Assert(err, qt.ErrorIs, provider.ErrOutcomeUnknown)
	default:
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	}
}
func historyRow(format protocol.HistoryScope_Format, version string) *protocol.HistoryRevision {
	row := &protocol.HistoryRevision{Version: version, Description: "history π\x00", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_DOWN,
		Applied: 1, Total: 3, Error: "failed", ErrorStatement: "SQL\nstatement", ExecutionNanoseconds: 123456789, Checksum: "exact checksum",
		AppliedAtSeconds: -62135596800, AppliedAtNanos: 999999999, OperatorVersion: "operator"}
	if format == protocol.HistoryScope_ATLAS {
		row.State = protocol.HistoryRevision_STATE_UNSPECIFIED
		row.Direction = protocol.HistoryRevision_DIRECTION_UNSPECIFIED
		row.AtlasType = 4294967295
		row.PartialHashes = []string{"first", "second"}
	}
	return row
}

func TestNativeHistorySubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	for _, format := range []protocol.HistoryScope_Format{protocol.HistoryScope_NATIVE, protocol.HistoryScope_ATLAS} {
		t.Run(format.String(), func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := inspectionFixture(c)
			table := path.Join(database, directory, "revisions")
			created := false
			c.Cleanup(func() {
				if created {
					_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
					c.Check(err, qt.IsNil)
				}
			})
			client, id, closeSession := historySession(c, binary, digest)
			scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "revisions", Format: format}
			missing := historyRead(c, client, scope)
			c.Assert(missing.Exists, qt.IsFalse)
			result, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_APPLIED)
			created = true
			c.Assert(historyRead(c, client, scope).Exists, qt.IsTrue)
			_, err = client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "verify"})
			c.Assert(err, qt.IsNil)
			version := "-9223372036854775808"
			if format == protocol.HistoryScope_ATLAS {
				version = ""
			}
			first := historyRow(format, version)
			historyChange(c, client, scope, nil, first, protocol.HistoryResult_APPLIED)
			read := historyRead(c, client, scope)
			c.Assert(read.Revisions, qt.HasLen, 1)
			c.Assert(proto.Equal(read.Revisions[0], first), qt.IsTrue)
			historyChange(c, client, scope, nil, first, protocol.HistoryResult_CONFLICT)
			stale := proto.CloneOf(first)
			stale.ExecutionNanoseconds++
			next := proto.CloneOf(first)
			next.Description = "replaced"
			historyChange(c, client, scope, stale, next, protocol.HistoryResult_CONFLICT)
			historyChange(c, client, scope, first, next, protocol.HistoryResult_APPLIED)
			historyChange(c, client, scope, first, nil, protocol.HistoryResult_CONFLICT)

			secondVersion := "9223372036854775807"
			if format == protocol.HistoryScope_ATLAS {
				secondVersion = "a\\' ` π"
			}
			second := historyRow(format, secondVersion)
			historyChange(c, client, scope, nil, second, protocol.HistoryResult_APPLIED)
			_, err = client.ReadHistory(c.Context(), &protocol.ReadHistoryRequest{Scope: scope, MaxRevisions: 1})
			c.Assert(err, qt.IsNotNil)
			closeSession()
			client.Close()
			fresh, newID, _ := historySession(c, binary, digest)
			freshScope := proto.CloneOf(scope)
			freshScope.SessionId = newID
			read = historyRead(c, fresh, freshScope)
			c.Assert(read.Revisions, qt.HasLen, 2)
			c.Assert(proto.Equal(read.Revisions[0], next), qt.IsTrue)
			c.Assert(proto.Equal(read.Revisions[1], second), qt.IsTrue)
			historyChange(c, fresh, freshScope, next, nil, protocol.HistoryResult_APPLIED)
			historyChange(c, fresh, freshScope, next, nil, protocol.HistoryResult_CONFLICT)
			read = historyRead(c, fresh, freshScope)
			c.Assert(read.Revisions, qt.HasLen, 1)
			c.Assert(proto.Equal(read.Revisions[0], second), qt.IsTrue)
		})
	}
}

func TestNativeHistorySharesExecutionTransaction(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	historyPath := path.Join(database, directory, "revisions")
	appPath := path.Join(database, directory, "application")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+appPath+"` (id Uint64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	created := false
	c.Cleanup(func() {
		if created {
			_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+historyPath+"`")
			c.Check(err, qt.IsNil)
		}
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+appPath+"`")
		c.Check(err, qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	_, err = client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	created = true
	other, otherID, _ := historySession(c, binary, digest)
	otherScope := proto.CloneOf(scope)
	otherScope.SessionId = otherID
	row := historyRow(scope.Format, "7")
	for _, action := range []protocol.FinishTransactionRequest_Action{protocol.FinishTransactionRequest_ROLLBACK, protocol.FinishTransactionRequest_COMMIT} {
		tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: action.String(), Isolation: protocol.TransactionIsolation_SERIALIZABLE})
		c.Assert(err, qt.IsNil)
		txScope := proto.CloneOf(scope)
		txScope.TransactionId = tx.Id
		historyChange(c, client, txScope, nil, row, protocol.HistoryResult_APPLIED)
		c.Assert(historyRead(c, client, txScope).Revisions, qt.HasLen, 1)
		c.Assert(historyRead(c, other, otherScope).Revisions, qt.HasLen, 0)
		c.Assert(executeSQL(c, client, id, tx.Id, "UPSERT INTO `"+appPath+"` (id) VALUES (1u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
		assertRowCount(c, opened, appPath, 0)
		_, err = client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: action})
		c.Assert(err, qt.IsNil)
		if action == protocol.FinishTransactionRequest_ROLLBACK {
			c.Assert(historyRead(c, client, scope).Revisions, qt.HasLen, 0)
			assertRowCount(c, opened, appPath, 0)
		} else {
			c.Assert(historyRead(c, client, scope).Revisions, qt.HasLen, 1)
			assertRowCount(c, opened, appPath, 1)
		}
	}
	// A malformed stored row refuses the entire read and prevents committing
	// application writes made before that history failure.
	_, err = opened.DB.ExecContext(c.Context(), "DECLARE $payload AS String; UPSERT INTO `"+historyPath+"` (version,revision) VALUES (99,$payload)", sql.Named("payload", []byte("corrupt")))
	c.Assert(err, qt.IsNil)
	tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: "corrupt", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	c.Assert(executeSQL(c, client, id, tx.Id, "UPSERT INTO `"+appPath+"` (id) VALUES (2u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
	txScope := proto.CloneOf(scope)
	txScope.TransactionId = tx.Id
	invalid, err := client.ReadHistory(c.Context(), &protocol.ReadHistoryRequest{Scope: txScope, MaxRevisions: 100})
	c.Assert(invalid, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
	finish, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(finish.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	assertRowCount(c, opened, appPath, 1)
}

func TestNativeHistoryRefusesForeignAndChangedStorage(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "foreign")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (version Int64 NOT NULL, description Utf8, PRIMARY KEY (version))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+table+"` (version,description) VALUES (1,\"preserve\")")
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "foreign", Format: protocol.HistoryScope_NATIVE}
	result, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_REFUSED)
	_, err = client.ReadHistory(c.Context(), &protocol.ReadHistoryRequest{Scope: scope, MaxRevisions: 100})
	c.Assert(err, qt.IsNotNil)
	historyChange(c, client, scope, nil, historyRow(scope.Format, "1"), protocol.HistoryResult_REFUSED)
	var description string
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT description FROM `"+table+"` WHERE version=1").Scan(&description), qt.IsNil)
	c.Assert(description, qt.Equals, "preserve")
	for _, schema := range []string{"../elsewhere", directory + "/../elsewhere"} {
		bad := proto.CloneOf(scope)
		bad.Schema = schema
		_, err = client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: bad, RequestId: fmt.Sprint("invalid-", schema)})
		c.Assert(err, qt.IsNotNil)
	}
}

func TestNativeHistoryConcurrentCASCannotOverwriteCommittedRow(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "revisions")
	created := false
	c.Cleanup(func() {
		if created {
			_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
			c.Check(err, qt.IsNil)
		}
	})
	binary, digest := providerBinary(c)
	first, firstID, _ := historySession(c, binary, digest)
	second, secondID, _ := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: firstID, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	_, err := first.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	created = true
	secondScope := proto.CloneOf(scope)
	secondScope.SessionId = secondID
	initial := historyRow(scope.Format, "1")
	historyChange(c, first, scope, nil, initial, protocol.HistoryResult_APPLIED)
	tx, err := second.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: secondID, TransactionId: "stale", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	staleScope := proto.CloneOf(secondScope)
	staleScope.TransactionId = tx.Id
	c.Assert(proto.Equal(historyRead(c, second, staleScope).Revisions[0], initial), qt.IsTrue)
	winner := proto.CloneOf(initial)
	winner.Description = "committed by first"
	historyChange(c, first, scope, initial, winner, protocol.HistoryResult_APPLIED)
	loser := proto.CloneOf(initial)
	loser.Description = "stale second write"
	result, mutationErr := second.MutateHistory(c.Context(), &protocol.MutateHistoryRequest{Scope: staleScope, RequestId: "stale-cas", Expected: initial, Replacement: loser})
	c.Assert(result, qt.IsNotNil)
	if result.Outcome == protocol.HistoryResult_APPLIED {
		c.Assert(mutationErr, qt.IsNil)
	} else {
		c.Assert(mutationErr, qt.IsNotNil)
	}
	finished, err := second.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: secondID, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(finished, qt.IsNotNil)
	c.Assert(err, qt.IsNotNil)
	c.Assert(finished.Outcome, qt.Not(qt.Equals), protocol.TransactionResult_COMMITTED)
	read := historyRead(c, first, scope)
	c.Assert(read.Revisions, qt.HasLen, 1)
	c.Assert(proto.Equal(read.Revisions[0], winner), qt.IsTrue)
}

func TestNativeHistoryChecksFormatAndLayoutOnEveryOperation(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "revisions")
	created := false
	c.Cleanup(func() {
		if created {
			_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
			c.Check(err, qt.IsNil)
		}
	})
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	_, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	created = true
	row := historyRow(scope.Format, "1")
	historyChange(c, client, scope, nil, row, protocol.HistoryResult_APPLIED)
	foreign := proto.CloneOf(scope)
	foreign.Format = protocol.HistoryScope_ATLAS
	result, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: foreign, RequestId: "wrong-format"})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_REFUSED)
	_, err = client.ReadHistory(c.Context(), &protocol.ReadHistoryRequest{Scope: foreign, MaxRevisions: 100})
	c.Assert(err, qt.IsNotNil)
	historyChange(c, client, foreign, nil, historyRow(foreign.Format, "1"), protocol.HistoryResult_REFUSED)
	c.Assert(proto.Equal(historyRead(c, client, scope).Revisions[0], row), qt.IsTrue)
	_, err = opened.DB.ExecContext(c.Context(), "ALTER TABLE `"+table+"` ADD COLUMN unexpected Utf8")
	c.Assert(err, qt.IsNil)
	_, err = client.ReadHistory(c.Context(), &protocol.ReadHistoryRequest{Scope: scope, MaxRevisions: 100})
	c.Assert(err, qt.IsNotNil)
	result, err = client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "changed-layout"})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_REFUSED)
	historyChange(c, client, scope, row, nil, protocol.HistoryResult_REFUSED)
	assertRowCount(c, opened, table, 1)
}

func TestNativeHistoryCreatesMissingDirectories(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	opened, directory, database := inspectionFixture(c)
	nested := path.Join(directory, "history", "nested")
	table := path.Join(database, nested, "revisions")
	client, id, _ := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: id, Schema: nested, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	c.Assert(historyRead(c, client, scope).Exists, qt.IsFalse)
	result, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "nested"})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.HistoryResult_APPLIED)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
		c.Check(opened.Driver.Scheme().RemoveDirectory(context.Background(), path.Join(database, nested)), qt.IsNil)
		c.Check(opened.Driver.Scheme().RemoveDirectory(context.Background(), path.Join(database, directory, "history")), qt.IsNil)
	})
	row := historyRow(scope.Format, "1")
	historyChange(c, client, scope, nil, row, protocol.HistoryResult_APPLIED)
	c.Assert(proto.Equal(historyRead(c, client, scope).Revisions[0], row), qt.IsTrue)
}
