//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// trustedRealm is one dev realm with two sessions on it: one the host opened
// for a run that does not trust its SQL, and one for a run that does.
type trustedRealm struct {
	client          *providerruntime.Client
	opened          *connection.Connection
	base            connection.Address
	root            string
	strict, trusted string
}

func newTrustedRealm(c *qt.C) trustedRealm {
	c.Helper()
	client := startProviderFor(c, provider.ReplayTrustFeature)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	realm := trustedRealm{client: client, opened: opened, base: base, root: root}
	realm.strict = openRealmSession(c, client, &protocol.OpenRequest{Engine: "ydb", Address: address})
	realm.trusted = openRealmSession(c, client, &protocol.OpenRequest{Engine: "ydb", Address: address, TrustedSql: true})
	return realm
}

// openRealmSession opens a session the test closes when it ends.
func openRealmSession(c *qt.C, client *providerruntime.Client, request *protocol.OpenRequest) string {
	c.Helper()
	session, err := client.Open(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.GetId()), qt.IsNil) })
	return session.GetId()
}

// outsideTable is the absolute path of a table at the test database's root,
// outside every realm, which the test drops when it ends.
func (r trustedRealm) outsideTable(c *qt.C, prefix string) string {
	c.Helper()
	table := path.Join(r.base.Database(), prefix+strings.ToLower(rand.Text())[:10])
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, r.opened.DB, "DROP TABLE IF EXISTS `"+table+"`"), qt.IsNil)
	})
	return table
}

func createTableStatement(table string) string {
	return "CREATE TABLE `" + table + "` (id Int64 NOT NULL, PRIMARY KEY (id))"
}

// The replay guard lifts a refusal whose only reason is the dev realm for a
// trusted scope, and keeps the refusal of a statement that moves the session's
// path prefix. Trust does not select the guard of a server the run owns: the
// owned server's guard accepts the prefix pragma, and the trusted guard of
// the dev database does not.
func TestTrustedReplayGuardSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c, provider.ReplayTrustFeature)
	statements := []string{createTableStatement("/local/outside"), "PRAGMA TablePathPrefix = \"/local\""}
	verdicts := func(scope *protocol.ReplayScope) []*protocol.ReplayVerdict {
		guarded, err := client.GuardReplay(c.Context(), &protocol.GuardReplayRequest{Engine: "ydb", Statements: statements, Scope: scope})
		c.Assert(err, qt.IsNil)
		return guarded.GetVerdicts()
	}

	strict := verdicts(&protocol.ReplayScope{Database: "/local"})
	trusted := verdicts(&protocol.ReplayScope{Database: "/local", TrustedSql: true})
	owned := verdicts(&protocol.ReplayScope{Database: "/local", Server: true, Provisioned: true})

	c.Assert(client.Description().GetEngines()[0].GetFeatures(), qt.Contains, provider.ReplayTrustFeature)
	c.Assert(strict[0].GetOperation(), qt.Equals, "the absolute path /local/outside")
	c.Assert(strict[1].GetOperation(), qt.Equals, "PRAGMA TablePathPrefix")
	c.Assert(trusted[0].GetRefused(), qt.IsFalse)
	c.Assert(trusted[1].GetOperation(), qt.Equals, "PRAGMA TablePathPrefix")
	c.Assert(owned[1].GetRefused(), qt.IsFalse)
}

// A host that does not list the feature cannot send trust: the SDK refuses
// the session before it reaches the provider, and the provider's engine does
// not declare the feature to that host.
func TestTrustedReplayWaitsForTheFeatureSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)

	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL"), TrustedSql: true})

	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
	c.Assert(session, qt.IsNil)
	c.Assert(client.Description().GetEngines()[0].GetFeatures(), qt.Not(qt.Contains), provider.ReplayTrustFeature)
}

// On one dev realm, a table at an absolute path outside it is refused on the
// session of a run that does not trust its SQL and created on the session of
// a run that does. A statement that moves the session's path prefix stays
// refused on both, and nothing it would create appears.
func TestTrustedRealmExecutionSubprocess(t *testing.T) {
	c := testcheck.New(t)
	realm := newTrustedRealm(c)
	outside := realm.outsideTable(c, "ptah_trust_outside_")
	moved := realm.outsideTable(c, "ptah_trust_moved_")

	refused := executeSQL(c, realm.client, realm.strict, "", createTableStatement(outside))
	applied := executeSQL(c, realm.client, realm.trusted, "", createTableStatement(outside))
	prefixed := executeSQL(c, realm.client, realm.trusted, "",
		"PRAGMA TablePathPrefix = \""+realm.base.Database()+"\"; "+createTableStatement(path.Base(moved)))

	c.Assert(refused.GetOutcome(), qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(refused.GetDiagnosticCode(), qt.Equals, "ydb.realm-escape")
	c.Assert(refused.GetMessage(), qt.Contains, "the absolute path "+outside)
	c.Assert(applied.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
	assertRowCount(c, realm.opened, outside, 0)
	c.Assert(prefixed.GetOutcome(), qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(prefixed.GetDiagnosticCode(), qt.Equals, "ydb.realm-escape")
	c.Assert(prefixed.GetMessage(), qt.Contains, "PRAGMA TablePathPrefix")
	c.Assert(childNames(c, realm.opened, realm.base.Database()), qt.Not(qt.Contains), path.Base(moved))
}

// A trusted run's operator statements are confined as its units are: the
// write outside the realm runs, and the prefix pragma does not.
func TestTrustedRealmStatementsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	realm := newTrustedRealm(c)
	outside := realm.outsideTable(c, "ptah_trust_statement_")
	c.Assert(executeSQL(c, realm.client, realm.trusted, "", createTableStatement(outside)).GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
	run := func(session, sql string) *protocol.StatementResult {
		result, err := realm.client.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: session, RequestId: "run", Sql: sql,
			Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 1})
		c.Assert(err, qt.IsNil)
		return result
	}

	strictWrite := run(realm.strict, "UPSERT INTO `"+outside+"` (id) VALUES (1)")
	trustedWrite := run(realm.trusted, "UPSERT INTO `"+outside+"` (id) VALUES (2)")
	trustedPrefix := run(realm.trusted, "PRAGMA TablePathPrefix = \""+realm.base.Database()+"\"")

	c.Assert(strictWrite.GetOutcome(), qt.Equals, protocol.StatementResult_REFUSED)
	c.Assert(strictWrite.GetRefusal(), qt.Contains, "the absolute path "+outside)
	c.Assert(trustedWrite.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(trustedPrefix.GetOutcome(), qt.Equals, protocol.StatementResult_REFUSED)
	c.Assert(trustedPrefix.GetRefusal(), qt.Contains, "PRAGMA TablePathPrefix")
	assertRowCount(c, realm.opened, outside, 1)
}

// The cleanup of a trusted run's realm removes what the run created in the
// realm and nothing else: the table the trusted SQL created outside the realm
// stays, as does a table no run created, and neither is listed.
func TestTrustedRealmCleanupSubprocess(t *testing.T) {
	c := testcheck.New(t)
	realm := newTrustedRealm(c)
	outside := realm.outsideTable(c, "ptah_trust_effect_")
	unrelated := realm.outsideTable(c, "ptah_trust_unrelated_")
	c.Assert(execFixtureSchema(c.Context(), realm.opened.DB, createTableStatement(unrelated)), qt.IsNil)
	for _, statement := range []string{createTableStatement("inside/rows"), createTableStatement(outside), "UPSERT INTO `inside/rows` (id) VALUES (1)"} {
		c.Assert(executeSQL(c, realm.client, realm.trusted, "", statement).GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
	}
	c.Assert(listedObjects(c, realm.client, realm.trusted), qt.DeepEquals, []string{"table inside/rows", "directory inside"})

	cleaned, err := realm.client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: realm.trusted, RequestId: "clean"})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED)
	c.Assert(listedObjects(c, realm.client, realm.trusted), qt.HasLen, 0)
	c.Assert(childNames(c, realm.opened, realm.root), qt.HasLen, 0)
	rootNames := childNames(c, realm.opened, realm.base.Database())
	c.Assert(rootNames, qt.Contains, path.Base(outside))
	c.Assert(rootNames, qt.Contains, path.Base(unrelated))
	assertRowCount(c, realm.opened, outside, 0)
	assertRowCount(c, realm.opened, unrelated, 0)
}
