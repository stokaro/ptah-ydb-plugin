//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// analyzeRefusal is the refusal of ANALYZE on a row table in either line's
// words: 25.1.4.7 refuses it while compiling the text, and 26.2.1.14, with
// default flags, in its execution stage. Each line answers one of them.
const analyzeRefusal = "(?s).*(analyze is not supported for oltp tables|ANALYZE command is not supported because `EnableColumnStatistics` feature flag is off).*"

// Ported from the host's TestYDBWriter_AnalyzeStopsAMigration: a migration
// file creates a table and then runs ANALYZE, which YDB refuses on both
// lines. ANALYZE changes no schema object and no row, so the refused unit
// applied nothing: the run stops at it as PARTIAL, one unit applied, with the
// line's words, and the table the first unit created stays. A unit that is
// ANALYZE alone is NOT_APPLIED. On 26.2 both read UNKNOWN without the rule,
// since YDB refuses ANALYZE there while running it, which says nothing of a
// scheme statement in general.
func TestAnalyzeRefusalAppliesNothingSubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newCommentFixture(c)
	analyze := "ANALYZE `" + path.Join(f.directory, "records") + "`;"
	created := "CREATE TABLE `" + path.Join(f.directory, "analyzed") + "` (id Int64 NOT NULL, PRIMARY KEY (id));"

	result, err := f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "migration", Units: []*protocol.Unit{
		{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: created}}, {ChangeId: "analyze", Body: &protocol.Unit_Sql{Sql: analyze}}}})

	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_PARTIAL, qt.Commentf("%s", result))
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	c.Assert(result.DiagnosticCode, qt.Equals, "ydb.statement-rejected")
	c.Assert(result.Message, qt.Matches, analyzeRefusal)
	c.Assert(rowCount(c, f.opened, path.Join(f.database, f.directory, "analyzed"), ""), qt.Equals, int64(0))

	alone, err := f.statement(c, analyze)

	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(alone.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED, qt.Commentf("%s", alone))
	c.Assert(alone.DiagnosticCode, qt.Equals, "ydb.statement-rejected")
	c.Assert(alone.Message, qt.Matches, analyzeRefusal)
}
