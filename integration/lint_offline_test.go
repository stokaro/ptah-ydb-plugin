//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// TestMigrationLintOfflineSubprocess drives migration lint through the
// executable and the SDK client, which validates every request and reply: the
// split of a migration directory, a check of its files under the YD rules, and
// an annotation of a split the host made without a dialect.
func TestMigrationLintOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	c.Assert(client.Description().Features, qt.Contains, provider.LintFeature)
	c.Assert(client.Description().Engines[0].LintRules, qt.HasLen, 26)

	first := "CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));\n"
	second := "-- a unique index over existing rows\nALTER TABLE t ADD INDEX t_v GLOBAL UNIQUE ON (v);\nDROP TOPIC events;\n" +
		"ALTER TABLE t DROP COLUMN v;\n"
	split, err := client.SplitLintStatements(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{
		{Name: "0001_t.up.sql", Sql: first, Version: 1, Up: true},
		{Name: "0002_t.up.sql", Sql: second, Version: 2, Up: true},
	}})
	c.Assert(err, qt.IsNil)
	c.Assert(split.Files, qt.HasLen, 2)
	c.Assert(split.Files[1].Statements, qt.HasLen, 3)
	c.Assert(split.Files[1].Statements[0].Comments, qt.DeepEquals, []string{"-- a unique index over existing rows"})
	c.Assert(split.Files[1].Statements[0].Effects[0].Fact, qt.Equals, protocol.LintFact_LINT_FACT_UNIQUE_INDEX)
	c.Assert(split.Files[1].Statements[1].Effects[0].Fact, qt.Equals, protocol.LintFact_LINT_FACT_STATEFUL_DROP)

	files := []*protocol.LintFile{
		{Name: "0001_t.up.sql", Version: 1, Up: true, Statements: split.Files[0].Statements},
		{Name: "0002_t.up.sql", Version: 2, Up: true, Statements: split.Files[1].Statements},
	}
	// The request answers no capability, so the target holds none: a unique
	// index added to a table that exists is refused.
	checked, err := client.CheckLintMigrations(c.Context(), &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD101"}, Files: files})
	c.Assert(err, qt.IsNil)
	c.Assert(checked.Findings, qt.HasLen, 1)
	c.Assert(checked.Findings[0].File, qt.Equals, int32(1))
	c.Assert(checked.Findings[0].Statement, qt.Equals, int32(0))

	// The index the earlier statement added keys v, which the history the
	// directory's files leave says.
	used, err := client.CheckLintMigrations(c.Context(), &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD104"}, Files: files})
	c.Assert(err, qt.IsNil)
	c.Assert(used.Findings, qt.HasLen, 1)
	c.Assert(used.Findings[0].Statement, qt.Equals, int32(2))
	c.Assert(used.Findings[0].Subjects[0].Name, qt.Equals, "v")

	annotated, err := client.AnnotateLintStatements(c.Context(), &protocol.AnnotateLintRequest{Engine: "ydb",
		Facts:   []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEFUL_DROP},
		Sources: []*protocol.LintSource{{Name: "0002_t.up.sql", Sql: second}}, Files: []*protocol.LintFile{files[1]}})
	c.Assert(err, qt.IsNil)
	c.Assert(annotated.Files[0].Statements[1].Effects[0].Kind, qt.Equals, "TOPIC")

	// Lint opens no session. A canceled call does not poison the process.
	canceled, cancel := context.WithCancel(c.Context())
	cancel()
	_, err = client.CheckLintMigrations(canceled, &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD101"}, Files: files})
	c.Assert(err, qt.IsNotNil)
	_, err = client.CheckLintMigrations(c.Context(), &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD999"}, Files: files})
	c.Assert(err, qt.IsNotNil)
	again, err := client.CheckLintMigrations(c.Context(), &protocol.CheckLintRequest{Engine: "ydb", Codes: []string{"YD101"}, Files: files})
	c.Assert(err, qt.IsNil)
	c.Assert(again.Findings, qt.HasLen, 1)
}

// startLintProvider starts the provider for a host that lists hostFeatures.
func startLintProvider(c *qt.C, hostFeatures ...string) *providerruntime.Client {
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

// kindsOf is each statement's kind and the facts it carries beside it.
func kindsOf(statements []*protocol.LintStatement) []string {
	result := []string{}
	for _, statement := range statements {
		var described []string
		for _, effect := range statement.Effects {
			switch effect.Fact {
			case protocol.LintFact_LINT_FACT_STATEMENT_KIND:
				described = append(described, effect.StatementClass.String()+" "+effect.Kind+" "+effect.Object)
			case protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY:
				described = append(described, "no key "+effect.Table)
			case protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY:
				described = append(described, "needs "+effect.Capability)
			}
		}
		result = append(result, strings.Join(described, "; "))
	}
	return result
}

const factsSQL = "CREATE TABLE notes (id Uint64, body Utf8);\nUPSERT INTO notes (id) VALUES (1);\nALTER TABLE notes ADD COLUMN at Int64 DEFAULT 0;\nDROP TABLE notes;\n"

// A host that lists lint.statement-facts reads what each statement is from
// the split and the annotation; one that does not list it is offered neither.
func TestStatementFactsOfflineSubprocess(t *testing.T) {
	want := []string{
		"LINT_STATEMENT_CLASS_CREATE_TABLE CREATE TABLE notes; no key notes",
		"LINT_STATEMENT_CLASS_OTHER UPSERT ",
		"LINT_STATEMENT_CLASS_ALTER_TABLE ALTER TABLE notes; needs add_column_with_default",
		"LINT_STATEMENT_CLASS_SCHEMA DROP TABLE notes",
	}
	for _, test := range []struct {
		name     string
		features []string
		listed   bool
		judged   []protocol.LintFact
		kinds    []string
	}{
		{name: "a host that lists the feature", features: []string{provider.LintStatementFactsFeature}, listed: true,
			judged: []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEMENT_KIND, protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY, protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY},
			kinds:  want},
		{name: "a host that does not", judged: []protocol.LintFact{}, kinds: []string{"", "", "", ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			client := startLintProvider(c, test.features...)

			split, err := client.SplitLintStatements(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{{Name: "x.sql", Sql: factsSQL}}})

			c.Assert(err, qt.IsNil)
			c.Assert(slices.Contains(client.Description().Engines[0].Features, provider.LintStatementFactsFeature), qt.Equals, test.listed)
			c.Assert(slices.DeleteFunc(slices.Clone(split.Judged), func(fact protocol.LintFact) bool { return !slices.Contains(test.judged, fact) }), qt.DeepEquals, test.judged)
			c.Assert(kindsOf(split.Files[0].Statements), qt.DeepEquals, test.kinds)
		})
	}
}

// An annotation answers the statement facts it is asked for, and a request
// for them from a host that did not list the feature is refused.
func TestStatementFactsAnnotationOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	facts := []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEMENT_KIND, protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY, protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY}
	listing := startLintProvider(c, provider.LintStatementFactsFeature)
	split, err := listing.SplitLintStatements(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{{Name: "x.sql", Sql: factsSQL}}})
	c.Assert(err, qt.IsNil)
	for _, statement := range split.Files[0].Statements {
		statement.Effects = nil
	}
	request := &protocol.AnnotateLintRequest{Engine: "ydb", Facts: facts, Sources: []*protocol.LintSource{{Name: "x.sql", Sql: factsSQL}},
		Files: []*protocol.LintFile{{Name: "x.sql", Statements: split.Files[0].Statements}}}

	annotated, err := listing.AnnotateLintStatements(c.Context(), request)
	c.Assert(err, qt.IsNil)
	answered := make([]*protocol.LintStatement, 0, len(annotated.Files[0].Statements))
	for _, annotation := range annotated.Files[0].Statements {
		answered = append(answered, &protocol.LintStatement{Effects: annotation.Effects})
	}
	c.Assert(kindsOf(answered), qt.DeepEquals, []string{
		"LINT_STATEMENT_CLASS_CREATE_TABLE CREATE TABLE notes; no key notes",
		"LINT_STATEMENT_CLASS_OTHER UPSERT ",
		"LINT_STATEMENT_CLASS_ALTER_TABLE ALTER TABLE notes; needs add_column_with_default",
		"LINT_STATEMENT_CLASS_SCHEMA DROP TABLE notes",
	})

	_, err = startLintProvider(c).AnnotateLintStatements(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `(?s).*unnegotiated.*`)
}
