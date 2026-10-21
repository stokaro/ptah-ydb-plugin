//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"google.golang.org/protobuf/proto"
)

// Lock qualification requires an isolated database. Refuse to modify an
// existing reserved node; cleanup owns only the node this test creates.
func lockFixture(c *qt.C) (*connection.Connection, string, string) {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), database)
	c.Assert(err, qt.IsNil)
	for _, entry := range listing.Children {
		c.Assert(entry.Name, qt.Not(qt.Equals), locking.NodeName, qt.Commentf("lock tests require an unused reserved node"))
	}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		listing, err := opened.Driver.Scheme().ListDirectory(ctx, database)
		c.Check(err, qt.IsNil)
		for _, entry := range listing.Children {
			if entry.Name == locking.NodeName {
				c.Check(opened.Driver.Coordination().DropNode(ctx, path.Join(database, locking.NodeName)), qt.IsNil)
			}
		}
	})
	return opened, directory, database
}
func nativeLockRequest(session, id, name string) *protocol.LockRequest {
	return &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: session, LockId: id, Name: name}, RequestId: rand.Text()}
}
func acquireNativeLock(c *qt.C, client *providerruntime.Client, request *protocol.LockRequest) {
	c.Helper()
	result, err := client.TryLock(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
}
func TestNativeLocksExcludeOtherProcessesAndReleaseOnClose(t *testing.T) {
	c := qt.New(t)
	_, _, _ = lockFixture(c)
	binary, digest := providerBinary(c)
	first, id, closeFirst := historySession(c, binary, digest)
	second, other, _ := historySession(c, binary, digest)
	name := "migration / π"
	request := nativeLockRequest(id, "first", name)
	acquisition, cancel := context.WithCancel(c.Context())
	result, err := first.TryLock(acquisition, request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
	cancel()
	checked, err := first.CheckLock(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(checked.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
	started := time.Now()
	busy, err := second.TryLock(c.Context(), nativeLockRequest(other, "busy", name))
	c.Assert(err, qt.ErrorIs, provider.ErrLockBusy)
	c.Assert(busy.Outcome, qt.Equals, protocol.LockResult_BUSY)
	c.Assert(time.Since(started) < 5*time.Second, qt.IsTrue, qt.Commentf("nonblocking acquisition took %s", time.Since(started)))
	independent := nativeLockRequest(other, "independent", "different lock")
	acquireNativeLock(c, second, independent)
	wrong := proto.CloneOf(request)
	wrong.Scope.Name = "different lock"
	_, err = first.ReleaseLock(c.Context(), wrong)
	c.Assert(err, qt.IsNotNil)
	checked, err = first.CheckLock(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(checked.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
	for range 2 {
		released, err := second.ReleaseLock(c.Context(), independent)
		c.Assert(err, qt.IsNil)
		c.Assert(released.Outcome, qt.Equals, protocol.LockResult_RELEASED)
	}
	closeFirst()
	takeover := nativeLockRequest(other, "takeover", name)
	acquireNativeLock(c, second, takeover)
	unused := nativeLockRequest(other, "late", "late lock")
	released, err := second.ReleaseLock(c.Context(), unused)
	c.Assert(err, qt.IsNil)
	c.Assert(released.Outcome, qt.Equals, protocol.LockResult_NOT_ACQUIRED)
	_, err = second.TryLock(c.Context(), unused)
	c.Assert(err, qt.IsNotNil)
	checked, err = second.CheckLock(c.Context(), takeover)
	c.Assert(err, qt.IsNil)
	c.Assert(checked.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
}
func TestNativeLockLossFencesExecutionHistoryAndCommit(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := lockFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	id := session.Id
	c.Cleanup(func() {
		// The server may confirm session expiration or end the stream without
		// a stop receipt. Repetition must retain whichever cleanup was observed.
		first := client.CloseSession(context.Background(), id)
		again := client.CloseSession(context.Background(), id)
		if first == nil {
			c.Check(again, qt.IsNil)
		} else {
			c.Check(first, qt.ErrorMatches, `.*\(Unavailable\)`)
			c.Assert(again, qt.IsNotNil)
			c.Check(again.Error(), qt.Equals, first.Error())
		}
	})
	request := nativeLockRequest(id, "owner", "migration")
	acquireNativeLock(c, client, request)
	table := path.Join(database, directory, "application")
	history := path.Join(database, directory, "revisions")
	c.Assert(executeSQL(c, client, id, "", "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, PRIMARY KEY(id))").Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_NATIVE}
	_, err = client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+history+"`")
		c.Check(err, qt.IsNil)
	})
	logs := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "events", Kind: protocol.MetadataScope_LOG}
	tags := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "tags", Kind: protocol.MetadataScope_TAGS}
	prepareMetadataStore(c, client, opened, database, logs)
	prepareMetadataStore(c, client, opened, database, tags)
	tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: "tx", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	c.Assert(executeSQL(c, client, id, tx.Id, "UPSERT INTO `"+table+"` (id) VALUES (1u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
	// An administrator removes the node while its owner is still alive.
	node := path.Join(database, locking.NodeName)
	c.Assert(opened.Driver.Coordination().DropNode(c.Context(), node), qt.IsNil)
	_, err = client.CheckLock(c.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(executeSQL(c, client, id, tx.Id, "UPSERT INTO `"+table+"` (id) VALUES (2u)").Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	txScope := proto.CloneOf(scope)
	txScope.TransactionId = tx.Id
	historyChange(c, client, txScope, nil, historyRow(scope.Format, "1"), protocol.HistoryResult_REFUSED)
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: logs, Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: logRecord(1)}}, protocol.MetadataResult_REJECTED)
	boundTags := proto.CloneOf(tags)
	boundTags.TransactionId = tx.Id
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: boundTags, Mutation: &protocol.MutateMetadataRequest_PutTag{PutTag: tagRecord("fenced", 1)}}, protocol.MetadataResult_REJECTED)

	_, err = client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.IsNotNil)
	_, err = client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	assertRowCount(c, opened, table, 0)
	c.Assert(historyRead(c, client, scope).Revisions, qt.HasLen, 0)
	for _, metadataScope := range []*protocol.MetadataScope{logs, tags} {
		records, exists, _ := drainMetadata(c, client, openMetadata(c, client, metadataScope), 1)
		c.Assert(exists, qt.IsTrue)
		c.Assert(records, qt.HasLen, 0)
	}

	// Native operations share the same permanent fence after lease loss.
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: database, Schema: directory, Name: "fenced-node"}
	desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureCoordinationChange("fenced", owner, nil, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.CoordinationOperationFromChange("fenced", change.Value)
	c.Assert(err, qt.IsNil)
	nativeResult, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: id, RequestId: "fenced-native", Units: []*protocol.Unit{{ChangeId: "fenced", Body: &protocol.Unit_Native{Native: operation.Value}}}})
	c.Assert(err, qt.IsNotNil)
	c.Assert(nativeResult.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(nativeResult.AppliedCount, qt.Equals, uint32(0))

	// Fresh ownership in another session does not repair the fenced owner.
	other, otherID, _ := historySession(c, binary, digest)
	acquireNativeLock(c, other, nativeLockRequest(otherID, "new", request.Scope.Name))
	c.Assert(executeSQL(c, client, id, "", "UPSERT INTO `"+table+"` (id) VALUES (3u)").Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
}
func TestNativeLocksRefuseRelaxedNodeWithoutChangingIt(t *testing.T) {
	c := qt.New(t)
	opened, _, database := lockFixture(c)
	node := path.Join(database, locking.NodeName)
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), node, coordination.NodeConfig{ReadConsistencyMode: coordination.ConsistencyModeRelaxed}), qt.IsNil)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	_, err := client.TryLock(c.Context(), nativeLockRequest(id, "refused", "migration"))
	c.Assert(err, qt.IsNotNil)
	_, config, err := opened.Driver.Coordination().DescribeNode(c.Context(), node)
	c.Assert(err, qt.IsNil)
	c.Assert(config.ReadConsistencyMode, qt.Equals, coordination.ConsistencyModeRelaxed)
}
func TestNativeLocksReleaseAfterProviderExit(t *testing.T) {
	c := qt.New(t)
	_, _, _ = lockFixture(c)
	binary, digest := providerBinary(c)
	owner := startProvider(c, binary, digest)
	session, err := owner.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	acquireNativeLock(c, owner, nativeLockRequest(session.Id, "owner", "process exit"))
	owner.Close()
	other, id, _ := historySession(c, binary, digest)
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		result, err := other.TryLock(ctx, nativeLockRequest(id, rand.Text(), "process exit"))
		if err == nil {
			c.Assert(result.Outcome, qt.Equals, protocol.LockResult_ACQUIRED)
			break
		}
		c.Assert(err, qt.ErrorIs, provider.ErrLockBusy)
		select {
		case <-ctx.Done():
			c.Fatal("dead provider did not release its semaphore")
		case <-time.After(200 * time.Millisecond):
		}
	}
}
