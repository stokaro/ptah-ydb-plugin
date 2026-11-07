package migrationlint_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// streamingQuery is a CREATE STREAMING QUERY as the host's embedded engine
// wrote one, with the clauses between CREATE and the name given.
func streamingQuery(clauses string) string {
	return "CREATE " + clauses + "`copy` WITH (RUN = TRUE, RESOURCE_POOL = `default`) AS DO BEGIN\nINSERT INTO sink SELECT * FROM source;\nEND DO;"
}

// safetyImpacts is the impact of each safety effect the split judged a
// statement to have, in order.
func safetyImpacts(statement *protocol.LintStatement) []protocol.LintImpact {
	var impacts []protocol.LintImpact
	for _, effect := range statement.GetEffects() {
		if effect.GetFact() == protocol.LintFact_LINT_FACT_SAFETY_EFFECT {
			impacts = append(impacts, effect.GetImpact())
		}
	}
	return impacts
}

// The split judges the safety effect of a statement only YDB knows the
// effect of, and leaves every other statement to the host's reading of its
// words: the host rates DROP VIEW destructive and a REVOKE or a membership
// removal a warning by their words, and a judgment here would take their
// place. A streaming query created with IF NOT EXISTS keeps its checkpoint
// even with OR REPLACE, so only a replacement without the guard loses state.
// Ported from the host's TestAssessSQL_RatesEachRemovalPerEngine (the YDB
// rows) and TestStreamingQueryCreateGuardsAgreeAcrossAssessments.
func TestSplit_JudgesOnlyWhatYDBKnows(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		want      []protocol.LintImpact
	}{
		{"a dropped view", "DROP VIEW `active_users`;", nil},
		{"a revoked permission", "REVOKE 'ydb.generic.read' ON `/local/users` FROM `reader`;", nil},
		{"a membership removal", "ALTER GROUP `readers` DROP USER `app`;", nil},
		{"a created streaming query", streamingQuery("STREAMING QUERY "), nil},
		{"a guarded streaming query", streamingQuery("STREAMING QUERY IF NOT EXISTS "), nil},
		{"a guarded replacement", streamingQuery("OR REPLACE STREAMING QUERY IF NOT EXISTS "), nil},
		{"a replacement", streamingQuery("OR REPLACE STREAMING QUERY "), []protocol.LintImpact{protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			split, err := migrationlint.SplitRequest(c.Context(), &protocol.SplitLintRequest{Engine: "ydb",
				Sources: []*protocol.LintSource{{Name: "0001_change.up.sql", Sql: test.sql, Version: 1, Up: true}}})
			c.Assert(err, qt.IsNil)
			c.Assert(split.Judged, qt.Contains, protocol.LintFact_LINT_FACT_SAFETY_EFFECT)
			c.Assert(split.Files, qt.HasLen, 1)
			c.Assert(split.Files[0].Statements, qt.HasLen, 1)
			c.Assert(safetyImpacts(split.Files[0].Statements[0]), qt.DeepEquals, test.want)
		})
	}
}
