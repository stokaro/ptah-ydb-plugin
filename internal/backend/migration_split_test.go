package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationunits"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
)

// splitHost is the context of a call from a host that lists the migration
// split.
func splitHost(c *qt.C) context.Context {
	c.Helper()
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.MigrationSplitFeature})
}

// TestDescriptionAdvertisesTheMigrationSplit pins the feature on the provider
// and on the ydb engine, beside the transactions and history it requires.
func TestDescriptionAdvertisesTheMigrationSplit(t *testing.T) {
	c := testcheck.New(t)

	description := backend.Description()

	c.Assert(provider.ValidateDescription(description, []string{provider.MigrationSplitFeature}), qt.IsNil)
	c.Assert(description.Features, qt.Contains, provider.MigrationSplitFeature)
	c.Assert(description.Engines[0].Features, qt.Contains, provider.MigrationSplitFeature)
	c.Assert(description.Engines[0].Features, qt.Contains, provider.TransactionsFeature)
	c.Assert(description.Engines[0].Features, qt.Contains, provider.HistoryFeature)
}

func TestSplitMigration_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.SplitMigrationRequest{Engine: "ydb",
		Body:       "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id));\n$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v);\nBATCH UPDATE t SET v = $v WHERE id > 0u;\n",
		Assertions: []string{"SELECT COUNT(*) = 1 FROM t", "UPSERT INTO t (id) VALUES (2u)"}}

	split, err := backend.New().SplitMigration(splitHost(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(split, qt.CmpEquals(protocmp.Transform()), &protocol.MigrationSplit{
		Units: []*protocol.MigrationUnit{
			{Sql: "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id))", Source: "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id));", Placement: protocol.MigrationUnit_OUTSIDE},
			{Sql: "$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v)", Source: "$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v);", Placement: protocol.MigrationUnit_WITH_CHECKPOINT},
			{Sql: "$v = \"a\"u;\nBATCH UPDATE t SET v = $v WHERE id > 0u", Source: "BATCH UPDATE t SET v = $v WHERE id > 0u;", Placement: protocol.MigrationUnit_OUTSIDE},
		},
		Assertions: []*protocol.AssertionVerdict{
			{ReadOnly: true},
			{Reason: "a YDB statement is proved read-only only when it is one SELECT"},
		},
	})
}

// TestSplitMigrationRefusesTheBody_FailurePath pins that a refused body still
// gets a verdict for each assertion, as the contract requires.
func TestSplitMigrationRefusesTheBody_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.SplitMigrationRequest{Engine: "ydb", Body: "UPSERT INTO t (id) VALUES (1u);\nCOMMIT;\n", Assertions: []string{"SELECT 1"}}

	split, err := backend.New().SplitMigration(splitHost(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(split, qt.CmpEquals(protocmp.Transform()), &protocol.MigrationSplit{
		Refusal:    &protocol.SplitRefusal{Start: 32, End: 39, Reason: migrationunits.TransactionControlReason},
		Assertions: []*protocol.AssertionVerdict{{ReadOnly: true}},
	})
}

func TestSplitMigrationRequest_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		host    []string
		request *protocol.SplitMigrationRequest
	}{
		{"not negotiated", nil, &protocol.SplitMigrationRequest{Engine: "ydb", Body: "SELECT 1"}},
		{"no body", []string{provider.MigrationSplitFeature}, &protocol.SplitMigrationRequest{Engine: "ydb"}},
		{"another engine", []string{provider.MigrationSplitFeature}, &protocol.SplitMigrationRequest{Engine: "postgres", Body: "SELECT 1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			split, err := backend.New().SplitMigration(provider.ContextWithHostFeatures(c.Context(), test.host), test.request)

			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(split, qt.IsNil)
		})
	}
}

// The verdict of an assertion is the refusal RunStatement gives the same text
// under READ_ONLY before it looks for a session, so a dry run refuses what the
// run refuses and passes what the run takes to the server.

func TestSplitMigrationVerdictAgreesWithTheRun_HappyPath(t *testing.T) {
	assertions := []string{"SELECT COUNT(*) = 2 FROM t", "SELECT 1;", "SELECT EXISTS (SELECT * FROM `dir/t` WHERE id = 1u)"}
	c := testcheck.New(t)
	p := backend.New()

	split, err := p.SplitMigration(splitHost(c), &protocol.SplitMigrationRequest{Engine: "ydb", Body: "SELECT 1", Assertions: assertions})

	c.Assert(err, qt.IsNil)
	for i, assertion := range assertions {
		c.Assert(split.Assertions[i], qt.CmpEquals(protocmp.Transform()), &protocol.AssertionVerdict{ReadOnly: true})
		run, err := p.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: "none", RequestId: "check", Sql: assertion, Access: protocol.RunStatementRequest_READ_ONLY, MaxRows: 1})
		c.Assert(status.Code(err), qt.Equals, codes.NotFound, qt.Commentf("%s reached the session lookup", assertion))
		c.Assert(run, qt.IsNil)
	}
}

func TestSplitMigrationVerdictAgreesWithTheRun_FailurePath(t *testing.T) {
	assertions := []string{
		"UPSERT INTO t (id) VALUES (1u)",
		"SELECT 1; SELECT 2",
		"SELECT * FROM t WHERE id IN (SELECT id FROM u) AND v = 'x' OR DELETE",
		"SELECT $PTAH_SECRET_TOKEN",
		"COMMENT ON TABLE t IS",
		"",
	}
	c := testcheck.New(t)
	p := backend.New()

	split, err := p.SplitMigration(splitHost(c), &protocol.SplitMigrationRequest{Engine: "ydb", Body: "SELECT 1", Assertions: assertions})

	c.Assert(err, qt.IsNil)
	c.Assert(split.Assertions, qt.HasLen, len(assertions))
	for i, assertion := range assertions[:len(assertions)-1] {
		run, err := p.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: "none", RequestId: "check", Sql: assertion, Access: protocol.RunStatementRequest_READ_ONLY, MaxRows: 1})
		c.Assert(err, qt.IsNil)
		c.Assert(run.Outcome, qt.Equals, protocol.StatementResult_REFUSED)
		c.Assert(split.Assertions[i], qt.CmpEquals(protocmp.Transform()), &protocol.AssertionVerdict{Reason: run.Refusal}, qt.Commentf("%s", assertion))
	}
	c.Assert(split.Assertions[len(assertions)-1].GetReadOnly(), qt.IsFalse)
}
