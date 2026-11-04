//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
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
