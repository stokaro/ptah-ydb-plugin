package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The description advertises lint on the provider and on the ydb engine, with
// a rule catalog the SDK accepts.
func TestDescriptionAdvertisesMigrationLint(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	c.Assert(description.Features, qt.Contains, provider.LintFeature)
	c.Assert(description.Engines[0].Features, qt.Contains, provider.LintFeature)
	c.Assert(description.Engines[0].LintRules, qt.Not(qt.HasLen), 0)
	c.Assert(provider.ValidateDescription(description, []string{provider.LintFeature}), qt.IsNil)
}

// lintFile is one up migration holding sql, split as the provider splits it.
func lintFile(c *qt.C, sql string) *protocol.LintFile {
	c.Helper()
	request := &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{{Name: "0001_t.up.sql", Sql: sql, Version: 1, Up: true}}}
	split, err := backend.New().SplitLintStatements(c.Context(), request)
	c.Assert(err, qt.IsNil)
	file := split.Files[0]
	file.Up, file.Version = true, 1
	return file
}

// A check reports each enabled rule's findings over the provider's own split,
// in a reply the SDK accepts.
func TestCheckLintMigrations_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD110", "YD111"},
		Files: []*protocol.LintFile{lintFile(c, "REVOKE GRANT OPTION FOR SELECT ON `t` FROM u;\nDROP USER u;\n")}}
	result, err := backend.New().CheckLintMigrations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateCheckedLint(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Findings, qt.HasLen, 2)
	c.Assert(result.Findings[0].Code, qt.Equals, "YD110")
	c.Assert(result.Findings[0].Statement, qt.Equals, int32(0))
	c.Assert(result.Findings[1].Code, qt.Equals, "YD111")
	c.Assert(result.Findings[1].Statement, qt.Equals, int32(1))
}

// A check that enables a code no rule carries, or names another engine, is
// refused whole, as is a canceled one.
func TestCheckLintMigrations_FailurePath(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		request *protocol.CheckLintRequest
		code    codes.Code
	}{
		{name: "unknown code", ctx: context.Background(), request: &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD999"}}, code: codes.InvalidArgument},
		{name: "another engine", ctx: context.Background(), request: &protocol.CheckLintRequest{Engine: "postgres", Codes: []string{"YD110"}}, code: codes.InvalidArgument},
		{name: "canceled", ctx: canceled, request: &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD110"},
			Files: []*protocol.LintFile{{Name: "0001_t.up.sql", Up: true}}}, code: codes.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := backend.New().CheckLintMigrations(test.ctx, test.request)
			c.Assert(status.Code(err), qt.Equals, test.code)
			c.Assert(result, qt.IsNil)
		})
	}
}

// An annotation answers every statement a host split, and a split judges
// every fact it reports, in replies the SDK accepts.
func TestAnnotateLintStatements_HappyPath(t *testing.T) {
	c := qt.New(t)
	file := lintFile(c, "DROP TOPIC t;\nSELECT 1;\n")
	request := &protocol.AnnotateLintRequest{Engine: "ydb", Facts: []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEFUL_DROP},
		Sources: []*protocol.LintSource{{Name: "0001_t.up.sql", Sql: "DROP TOPIC t;\nSELECT 1;\n"}}, Files: []*protocol.LintFile{file}}
	result, err := backend.New().AnnotateLintStatements(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateAnnotatedLint(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Files[0].Statements, qt.HasLen, 2)
	c.Assert(result.Files[0].Statements[0].Effects[0].Fact, qt.Equals, protocol.LintFact_LINT_FACT_STATEFUL_DROP)
	c.Assert(result.Files[0].Statements[1].Effects, qt.HasLen, 0)
}

// A request that does not pair its sources with statement lists is refused.
func TestAnnotateLintStatements_FailurePath(t *testing.T) {
	c := qt.New(t)
	request := &protocol.AnnotateLintRequest{Engine: "ydb", Facts: []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEFUL_DROP},
		Files: []*protocol.LintFile{{Name: "0001_t.up.sql"}}}
	result, err := backend.New().AnnotateLintStatements(c.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(result, qt.IsNil)
}
