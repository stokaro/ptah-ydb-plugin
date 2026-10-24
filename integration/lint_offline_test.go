//go:build integration

package integration_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
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
