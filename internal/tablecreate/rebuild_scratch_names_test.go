package tablecreate_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// scratchNamedTable is a table at app/name beside app.records: one the declaration
// holds and the plan leaves as it is, or one the plan drops, which holds the
// key id.
func scratchNamedTable(name string, action protocol.PlanningTable_Action) *protocol.PlanningTable {
	subject := tableSubject()
	subject.Name = part(name)
	table := &protocol.PlanningTable{Subject: subject, Action: action}
	if action == protocol.PlanningTable_DROP {
		table.Current = &protocol.ObservedTable{Name: name, Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "id", PrimaryKey: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}}}
		return table
	}
	table.Desired = &protocol.DeclaredTable{Name: name, Schema: "app", Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
	return table
}

// rebuildStatementHeads keeps each statement up to the list its columns open, so a
// CREATE TABLE and an INSERT read as the table they write.
func rebuildStatementHeads(statements []string) []string {
	heads := make([]string, 0, len(statements))
	for _, statement := range statements {
		head, _, _ := strings.Cut(statement, " (")
		heads = append(heads, head)
	}
	return heads
}

// A rebuild creates its new table under __ptah_rebuild_<table> and moves the
// old one to __ptah_replaced_<table>, as the host's embedded engine names
// them. Where a table the declaration holds, or one the plan drops, already
// has such a name, the rebuild takes the next free one, numbered from 1, so it
// never creates or renames a table onto one the plan also holds. The table the
// plan drops keeps its own name. Ported from the host's
// TestGenerateMigrationAST_TableRebuild_PicksAFreeScratchName.
func TestRebuildPlanPicksAFreeScratchName(t *testing.T) {
	for _, test := range []struct {
		name              string
		declared, dropped []string
		statements        []string
	}{
		{name: "a declared table and a dropped table under the two names", declared: []string{"__ptah_rebuild_records"}, dropped: []string{"__ptah_replaced_records"},
			statements: []string{"DROP TABLE `app/__ptah_replaced_records`;",
				"CREATE TABLE `app/__ptah_rebuild_records_1`",
				"INSERT INTO `app/__ptah_rebuild_records_1`",
				"ALTER TABLE `app/records` RENAME TO `app/__ptah_replaced_records_1`;",
				"ALTER TABLE `app/__ptah_rebuild_records_1` RENAME TO `app/records`;",
				"DROP TABLE `app/__ptah_replaced_records_1`;"}},
		{name: "the first numbered name taken too", declared: []string{"__ptah_rebuild_records", "__ptah_rebuild_records_1"},
			statements: []string{
				"CREATE TABLE `app/__ptah_rebuild_records_2`",
				"INSERT INTO `app/__ptah_rebuild_records_2`",
				"ALTER TABLE `app/records` RENAME TO `app/__ptah_replaced_records`;",
				"ALTER TABLE `app/__ptah_rebuild_records_2` RENAME TO `app/records`;",
				"DROP TABLE `app/__ptah_replaced_records`;"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
			for _, name := range test.declared {
				request.Tables = append(request.Tables, scratchNamedTable(name, protocol.PlanningTable_CONTEXT))
			}
			for _, name := range test.dropped {
				request.Tables = append(request.Tables, scratchNamedTable(name, protocol.PlanningTable_DROP))
			}

			plan, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(rebuildStatementHeads(renderedText(c, plan, request.Capabilities)), qt.DeepEquals, test.statements)
		})
	}
}
