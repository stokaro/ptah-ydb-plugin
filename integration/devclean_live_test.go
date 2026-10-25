//go:build integration

package integration_test

import (
	"context"
	"net/url"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// startProviderFor starts the provider for a host that lists hostFeatures.
func startProviderFor(c *qt.C, hostFeatures ...string) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin", HostFeatures: hostFeatures,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// realmSession opens a session on a dev realm of its own in the test
// database, and removes the realm when the test ends.
func realmSession(c *qt.C, client *providerruntime.Client) (session, root string, opened *connection.Connection) {
	c.Helper()
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err = connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	opening, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), opening.Id), qt.IsNil) })
	return opening.Id, root, opened
}

func listedObjects(c *qt.C, client *providerruntime.Client, session string, schemas ...string) []string {
	c.Helper()
	listed, err := client.ListDevObjects(c.Context(), &protocol.ListDevObjectsRequest{SessionId: session, Schemas: schemas})
	c.Assert(err, qt.IsNil)
	objects := make([]string, len(listed.GetObjects()))
	for i, object := range listed.GetObjects() {
		objects[i] = object.GetObject().GetKind() + " " + path.Join(object.GetObject().GetSchema(), object.GetObject().GetName())
	}
	return objects
}

// TestDevCleanupSubprocess lists and empties a dev realm that holds tables,
// a view, a topic, a coordination node and nested directories, and leaves
// alone the lock the session holds.
func TestDevCleanupSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	for _, statement := range []string{
		"CREATE TABLE `t` (id Int64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE `app/sub/rows` (id Int64 NOT NULL, PRIMARY KEY (id))",
		"CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS SELECT id FROM `t`",
		"CREATE TOPIC `queues/events`",
	} {
		c.Assert(executeSQL(c, client, session, "", statement).Outcome, qt.Equals, protocol.Execution_APPLIED)
	}
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(root, "nodes")), qt.IsNil)
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), path.Join(root, "nodes/workers"), coordination.NodeConfig{
		SelfCheckPeriodMillis: 1000, SessionGracePeriodMillis: 10000,
		ReadConsistencyMode: coordination.ConsistencyModeRelaxed, AttachConsistencyMode: coordination.ConsistencyModeStrict,
		RatelimiterCountersMode: coordination.RatelimiterCountersModeAggregated,
	}), qt.IsNil)
	acquired, err := client.TryLock(c.Context(), &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: session, Name: "cleanup", LockId: "held"}, RequestId: "lock"})
	c.Assert(err, qt.IsNil)
	c.Assert(acquired.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)

	c.Assert(listedObjects(c, client, session), qt.DeepEquals, []string{
		"table app/sub/rows", "directory app/sub", "view app/v", "directory app",
		"coordination node nodes/workers", "directory nodes",
		"topic queues/events", "directory queues", "table t",
	})
	c.Assert(listedObjects(c, client, session, "app"), qt.DeepEquals, []string{"table app/sub/rows", "directory app/sub", "view app/v"})

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "clean"})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED)
	c.Assert(listedObjects(c, client, session), qt.HasLen, 0)
	held, err := client.CheckLock(c.Context(), &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: session, Name: "cleanup", LockId: "held"}, RequestId: "check"})
	c.Assert(err, qt.IsNil)
	c.Assert(held.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(childNames(c, opened, root), qt.DeepEquals, []string{"ptah_locks"})
	released, err := client.ReleaseLock(c.Context(), &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: session, Name: "cleanup", LockId: "held"}, RequestId: "release"})
	c.Assert(err, qt.IsNil)
	c.Assert(released.GetOutcome(), qt.Equals, protocol.LockResult_RELEASED)
	c.Assert(opened.Driver.Coordination().DropNode(c.Context(), path.Join(root, "ptah_locks")), qt.IsNil)
}

// TestDevRealmProvisionSubprocess creates a realm of the run's own in the
// test database for a host that lists database.dev-realms, opens it as a
// database of its own, and removes it with everything in it on release.
func TestDevRealmProvisionSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c, provider.DevRealmsFeature)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	opened, err := connection.Open(c.Context(), base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	c.Assert(client.Description().GetEngines()[0].GetFeatures(), qt.Contains, provider.DevRealmsFeature)

	reply, err := client.ProvisionDevDatabase(c.Context(), &protocol.ProvisionDevRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})

	c.Assert(err, qt.IsNil)
	c.Assert(reply.GetRefusal(), qt.IsNil)
	realmURL, err := url.Parse(reply.GetUrl())
	c.Assert(err, qt.IsNil)
	name := realmURL.Query().Get("dev_realm")
	root := path.Join(base.Database(), connection.RealmDirectory, name)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: reply.GetUrl()})
	c.Assert(err, qt.IsNil)
	c.Assert(session.GetDatabase(), qt.Equals, root)
	c.Assert(executeSQL(c, client, session.GetId(), "", "CREATE TABLE `kept/rows` (id Int64 NOT NULL, PRIMARY KEY (id))").Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(client.CloseSession(c.Context(), session.GetId()), qt.IsNil)

	_, err = client.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.GetLease()})

	c.Assert(err, qt.IsNil)
	c.Assert(childNames(c, opened, path.Join(base.Database(), connection.RealmDirectory)), qt.Not(qt.Contains), name)
}

// TestDevRealmAddressWaitsForTheFeatureSubprocess pins that a host that does
// not list database.dev-realms is not offered realms: the address does not
// travel, and the engine does not declare the feature.
func TestDevRealmAddressWaitsForTheFeatureSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c)

	_, err := client.ProvisionDevDatabase(c.Context(), &protocol.ProvisionDevRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})

	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
	c.Assert(client.Description().GetEngines()[0].GetFeatures(), qt.Not(qt.Contains), provider.DevRealmsFeature)
}

// TestGuardReplayOfflineSubprocess asks the replay guard, without a session,
// about statements under the scope of a dev database and of a server the run
// owns.
func TestGuardReplayOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c)
	statements := []string{"CREATE TABLE t (id Int64, PRIMARY KEY (id))", "CREATE USER u PASSWORD 'p'", "CREATE TRANSFER x FROM a TO b USING ($m) -> { RETURN $m; }"}

	database, err := client.GuardReplay(c.Context(), &protocol.GuardReplayRequest{Engine: "ydb", Statements: statements, Scope: &protocol.ReplayScope{}})
	c.Assert(err, qt.IsNil)
	server, err := client.GuardReplay(c.Context(), &protocol.GuardReplayRequest{Engine: "ydb", Statements: statements, Scope: &protocol.ReplayScope{Server: true, Provisioned: true}})
	c.Assert(err, qt.IsNil)

	c.Assert(database.GetVerdicts(), qt.CmpEquals(protocmp.Transform()), []*protocol.ReplayVerdict{
		{}, {Refused: true, Operation: "a user of the whole database"}, {Refused: true, Operation: "a transfer"},
	})
	c.Assert(server.GetVerdicts(), qt.CmpEquals(protocmp.Transform()), []*protocol.ReplayVerdict{
		{}, {}, {Refused: true, Operation: "a transfer"},
	})
}

// TestStatementTimeoutSubprocess stops units at the statement timeout of a
// host that lists units.statement-timeout: a data query in the caller's
// transaction, of which nothing is applied, and an index build, which the
// provider cancels through the operation service or finds completed.
func TestStatementTimeoutSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c, provider.StatementTimeoutFeature)
	session, root, opened := realmSession(c, client)
	c.Assert(executeSQL(c, client, session, "", "CREATE TABLE `rows` (id Uint64 NOT NULL, v Uint64, PRIMARY KEY (id))").Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, "rows")+"`"), qt.IsNil)
	})
	fill := "UPSERT INTO `rows` SELECT * FROM AS_TABLE(ListMap(ListFromRange(0ul, 300000ul), ($i) -> (<|id: $i, v: $i * 7ul|>)))"
	c.Assert(executeSQL(c, client, session, "", fill).Outcome, qt.Equals, protocol.Execution_APPLIED)

	_, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session, TransactionId: "slow", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	inside, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session, RequestId: "slow", TransactionId: "slow", StatementTimeoutNanoseconds: int64(5 * time.Millisecond),
		Units: []*protocol.Unit{{ChangeId: "copy", Body: &protocol.Unit_Sql{Sql: "UPSERT INTO `rows` SELECT id + 1000000ul AS id, v FROM `rows`"}}}})
	c.Assert(err, qt.ErrorIs, provider.ErrStatementTimeout)
	c.Assert(inside.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(inside.GetStatementTimedOut(), qt.IsTrue)
	finished, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session, TransactionId: "slow", Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(finished.GetOutcome(), qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	assertRowCount(c, opened, path.Join(connection.RealmDirectory, path.Base(root), "rows"), 300000)

	build, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session, RequestId: "build", StatementTimeoutNanoseconds: int64(50 * time.Millisecond),
		Units: []*protocol.Unit{{ChangeId: "index", Body: &protocol.Unit_Sql{Sql: "ALTER TABLE `rows` ADD INDEX by_v GLOBAL SYNC ON (v)"}}}})
	indexes := childNames(c, opened, path.Join(root, "rows"))
	c.Logf("the index build at the statement timeout: %s", build.GetOutcome())

	c.Assert([]protocol.Execution_Outcome{protocol.Execution_APPLIED, protocol.Execution_NOT_APPLIED}, qt.Contains, build.GetOutcome(), qt.Commentf("%v", err))
	c.Assert(build.GetStatementTimedOut(), qt.Equals, build.GetOutcome() == protocol.Execution_NOT_APPLIED)
	c.Assert(slices.Contains(indexes, "by_v"), qt.Equals, build.GetOutcome() == protocol.Execution_APPLIED)
}

// childNames lists the names a directory, or a table's index directory,
// holds, and none for a path that does not exist.
func childNames(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), absolute)
	names := []string{}
	if err != nil {
		return names
	}
	for _, entry := range listing.Children {
		names = append(names, entry.Name)
	}
	return names
}
