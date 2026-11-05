//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// executeFailing runs statement alone through client, which must fail it, and
// returns the result and the error.
func executeFailing(c *qt.C, client *providerruntime.Client, session, statement string) (*protocol.Execution, error) {
	c.Helper()
	result, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session, RequestId: "reasons",
		Units: []*protocol.Unit{{ChangeId: "0", Body: &protocol.Unit_Sql{Sql: statement}}}})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	return result, err
}

// A unit YDB refused says why to a client that lists errors.reasons, in the
// provider's words: what YDB refused, its status, the issue code and the
// position YDB gave. YDB's issue text quotes a token of the statement, a
// literal in it and, on 26.2, the table's path, and none of it reaches the
// error. A client that does not list the feature reads the outcome alone.
func TestRefusedUnitGivesItsReasonSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "reasons")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+table+"` (id Int64 NOT NULL, v Utf8 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		c.Check(execFixtureSchema(context.Background(), opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+table+"` (id, v) VALUES (1, 'kept'u)")
	c.Assert(err, qt.IsNil)
	listing := startProviderFor(c, provider.ErrorReasonsFeature)
	listed, err := listing.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(listing.CloseSession(context.Background(), listed.Id), qt.IsNil) })
	unlisting := startProviderFor(c)
	unlisted, err := unlisting.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(unlisting.CloseSession(context.Background(), unlisted.Id), qt.IsNil) })
	unknownType := "CREATE TABLE `" + table + "_b` (id NoSuchType NOT NULL, PRIMARY KEY (id))"
	duplicate := "INSERT INTO `" + table + "` (id, v) VALUES (1, 'hunter2-secret'u)"
	literal := "CREATE USER reasons_user PASSWORD 'hunter2-secret' 'extra'"

	typeResult, typeErr := executeFailing(c, listing, listed.Id, unknownType)
	duplicateResult, duplicateErr := executeFailing(c, listing, listed.Id, duplicate)
	literalResult, literalErr := executeFailing(c, listing, listed.Id, literal)
	unlistedResult, unlistedErr := executeFailing(c, unlisting, unlisted.Id, duplicate)

	c.Assert(typeResult.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(typeErr, qt.ErrorMatches,
		`provider execution: NOT_APPLIED \(0 units confirmed applied\): YDB refused the statement while compiling it \(GENERIC_ERROR, line 1, column \d+\)`)
	c.Assert(duplicateResult.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(duplicateErr.Error(), qt.Equals, "provider execution: NOT_APPLIED (0 units confirmed applied): "+
		"YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)")
	c.Assert(literalResult.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(literalErr, qt.ErrorMatches,
		`provider execution: NOT_APPLIED \(0 units confirmed applied\): YDB refused the statement while compiling it \(GENERIC_ERROR, line 1, column \d+\)`)
	// The result's message is YDB's issue text, which quotes what the reason
	// leaves out. 26.2 also names the table's path beside the key conflict;
	// 25.1 does not.
	c.Assert(typeResult.GetMessage(), qt.Contains, "NoSuchType")
	c.Assert(duplicateResult.GetMessage(), qt.Contains, "Conflict with existing key")
	c.Assert(literalResult.GetMessage(), qt.Contains, "'extra'")
	c.Assert(typeErr.Error()+duplicateErr.Error()+literalErr.Error(), qt.Not(qt.Contains), "NoSuchType")
	c.Assert(typeErr.Error()+duplicateErr.Error()+literalErr.Error(), qt.Not(qt.Contains), directory)
	c.Assert(typeErr.Error()+duplicateErr.Error()+literalErr.Error(), qt.Not(qt.Contains), "extra")
	c.Assert(unlistedResult.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(unlistedResult.GetDiagnostic(), qt.IsNil)
	c.Assert(unlistedErr.Error(), qt.Equals, "provider execution: NOT_APPLIED (0 units confirmed applied)")
	assertRowCount(c, opened, table, 1)
}

// A unit YDB refuses because a feature flag is off names the capability and
// the flag in the host's embedded engine's words, around the provider's own
// reason, and not YDB's text. A resource pool is refused on every line, which
// runs with EnableResourcePools off by default. A session whose address names
// the monitoring endpoint read the flags, so the sentence asks only to turn
// the flag on; one without it took the line's default, and is told to name
// the endpoint too.
func TestRefusedUnitNamesTheFeatureFlagSubprocess(t *testing.T) {
	const refusal = "provider execution: NOT_APPLIED (0 units confirmed applied): capability resource_pools is off on this YDB cluster, " +
		"which runs with feature flag EnableResourcePools off: YDB refused the statement (UNSUPPORTED, issue 2030). Turn the flag on"
	for _, test := range []struct {
		name, want string
		monitoring bool
	}{
		{"the flags read", refusal, true},
		{"the line's default", refusal + ", or name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			client := startProviderFor(c, provider.ErrorReasonsFeature)
			session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: flagReasonAddress(c, test.monitoring)})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
			c.Assert(session.GetCapabilities()["resource_pools"], qt.IsFalse)

			result, err := executeFailing(c, client, session.Id, "CREATE RESOURCE POOL `ptah_flag_reason_pool` WITH (CONCURRENT_QUERY_LIMIT = 1)")

			c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(err.Error(), qt.Equals, test.want)
			c.Assert(result.GetMessage(), qt.Contains, "Resource pools are disabled")
			c.Assert(err.Error(), qt.Not(qt.Contains), "Resource pools are disabled")
		})
	}
}

// flagReasonAddress is the test address, which names the monitoring endpoint,
// or the same address without the endpoint.
func flagReasonAddress(c *qt.C, monitoring bool) string {
	c.Helper()
	address := testAddress(c, nil, "", "")
	if monitoring {
		return address
	}
	return withoutMonitoring(c, address)
}

// skipOffLine skips a test that measures what one release line answers on a
// server of another line.
func skipOffLine(c *qt.C, version, line string) {
	c.Helper()
	if got := serverLine(c, version); got != line {
		c.Skipf("the server runs YDB %s; this test measures %s", got, line)
	}
}

// 25.1 has no unique secondary index, and refuses one inside its execution
// stage, before it proposes a schema operation, so nothing of the statement
// applied. The unit is NOT_APPLIED, with the provider's reason for it, as the
// host's embedded engine names the refusal; the table keeps no index.
func TestUniqueIndexRefusedBeforeItsOperationSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "unique_refused")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+table+"` (id Int64 NOT NULL, v Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		c.Check(execFixtureSchema(context.Background(), opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	client := startProviderFor(c, provider.ErrorReasonsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	skipOffLine(c, session.GetServerVersion(), "25.1")

	result, err := executeFailing(c, client, session.Id, "ALTER TABLE `"+table+"` ADD INDEX `u` GLOBAL UNIQUE SYNC ON (`v`)")

	c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(result.GetDiagnosticCode(), qt.Equals, "ydb.statement-rejected")
	c.Assert(err, qt.ErrorMatches, `provider execution: NOT_APPLIED \(0 units confirmed applied\): YDB refused the statement: the YDB release the cluster runs has no unique `+
		`secondary index \(GENERIC_ERROR, issue 1060, line 1, column \d+\)`)
	c.Assert(result.GetMessage(), qt.Contains, "Unknown index type: syncGlobalUnique")
	c.Assert(heldIndexKinds(c, opened, table), qt.HasLen, 0)
}
