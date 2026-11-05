//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// columnStep is a host step that adds or drops a column of records, with the
// footprint the host gives a column step.
func (f *facetFixture) columnStep(id, name string, add bool) *protocol.CommonPlanningStep {
	table := testplanning.Subject(f.owner)
	column := testplanning.Subject(&protocol.Reference{Engine: "ydb", Kind: "column", Database: f.database, Schema: f.directory, Path: []string{"records"}, Name: name})
	column.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
	step := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: id}, Parent: proto.CloneOf(table),
		Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{
			{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: column, Action: protocol.PlanningEffect_DROP}}}}
	if add {
		step.AddedColumn = &protocol.PlanningColumn{Name: name, Type: "Utf8", Nullable: true}
		step.Metadata.Effects[1].Action = protocol.PlanningEffect_CREATE
	}
	return step
}

func (f *facetFixture) heldColumns(c *qt.C) []string {
	c.Helper()
	var names []string
	for _, column := range observedTable(c, f.client, f.session, f.directory).Columns {
		names = append(names, column.Name)
	}
	return names
}

// TestIndexAlterationDropsTheIndexBeforeTheColumnItReads removes a column and
// the index that reads it in one plan. The server refuses the column removal
// while the index exists, so the index step has to come first.
func TestIndexAlterationDropsTheIndexBeforeTheColumnItReads(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
	fixture.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}}
	fixture.steps = []*protocol.CommonPlanningStep{fixture.columnStep("drop-payload", "payload", false)}
	declared := declaredIndexes()
	found := fixture.compareIndexes(c, declared)
	c.Assert(len(found.Changes), qt.Equals, 1)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(len(plan.Diagnostics), qt.Equals, 0, qt.Commentf("%s", plan))
	var names []string
	for _, step := range scheduled(c, plan) {
		names = append(names, step.Id.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-drop-column/000000"})
	execution := fixture.execute(c, "index-and-column-drop", plan)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(len(fixture.heldIndexes(c)), qt.Equals, 0)
	c.Assert(fixture.heldColumns(c), qt.DeepEquals, []string{"id", "ts"})
}

// TestIndexAlterationAddsTheIndexAfterTheColumnItReads adds a column and an
// index on it in one plan.
func TestIndexAlterationAddsTheIndexAfterTheColumnItReads(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	fixture.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true},
		{Name: "payload", Type: "Utf8", Nullable: true}, {Name: "label", Type: "Utf8", Nullable: true}}
	fixture.steps = []*protocol.CommonPlanningStep{fixture.columnStep("add-label", "label", true)}
	declared := declaredIndexes(indexSpec("by_label", "globalIndex", `["label"]`, ""))
	found := fixture.compareIndexes(c, declared)
	c.Assert(len(found.Changes), qt.Equals, 1)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(len(plan.Diagnostics), qt.Equals, 0, qt.Commentf("%s", plan))
	var names []string
	for _, step := range scheduled(c, plan) {
		names = append(names, step.Id.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"row-table-add-column/000000", "row-table-add-index/000000"})
	execution := fixture.execute(c, "column-and-index-add", plan)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(fixture.heldIndexes(c)["by_label"].IndexColumns, qt.DeepEquals, []string{"label"})
}

// TestIndexAlterationRefusesTheColumnOfAKeptIndex drops a column an index that
// stays still reads; the plan is refused before any step runs.
func TestIndexAlterationRefusesTheColumnOfAKeptIndex(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
	fixture.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}}
	fixture.steps = []*protocol.CommonPlanningStep{fixture.columnStep("drop-payload", "payload", false)}
	declared := declaredIndexes(indexSpec("by_payload", "globalIndex", `["payload"]`, ""))
	plan := fixture.plan(c, nil, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(len(plan.Diagnostics), qt.Equals, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `index "by_payload" reads column "payload", which the declaration does not state`)
	c.Assert(len(plan.Contributions), qt.Equals, 0)
}

// TestIndexAlterationUniqueSubprocess adds a unique index to a table that
// exists, on a session that establishes it can: a 26.2 server started with
// enable_add_unique_index.
func TestIndexAlterationUniqueSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	requireSessionCapability(c, fixture.session, tablecreate.UniqueIndexOnExistingTableCapability, true)
	declared := declaredIndexes(indexSpec("by_payload", "globalUniqueIndex", `["payload"]`, ""))
	found := fixture.compareIndexes(c, declared)
	c.Assert(len(found.Changes), qt.Equals, 1)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(len(plan.Diagnostics), qt.Equals, 0, qt.Commentf("%s", plan))
	execution, err := fixture.tryExecute(c, "unique-add", plan)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(fixture.heldIndexes(c)["by_payload"].GetGlobalUniqueIndex(), qt.IsNotNil)
	c.Assert(len(fixture.compareIndexes(c, declared).Changes), qt.Equals, 0)
}

// TestIndexAlterationUniqueRefusedSubprocess is the host's
// TestYDBUniqueConstraint_AddedToATableThatExists_FailurePath for the index
// YDB holds a UNIQUE as. A session without unique_index_on_existing_table,
// as on 25.1, or on a later line with EnableAddUniqueIndex off, cannot add a
// unique index to a table that exists: YDB answers `Adding a unique index to
// an existing table is disabled` after the call, with an outcome the provider
// cannot confirm. The plan refuses it first, in the host's words, and the
// table keeps its indexes.
func TestIndexAlterationUniqueRefusedSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	requireSessionCapability(c, fixture.session, tablecreate.UniqueIndexOnExistingTableCapability, false)
	declared := declaredIndexes(indexSpec("by_payload", "globalUniqueIndex", `["payload"]`, ""))
	found := fixture.compareIndexes(c, declared)
	c.Assert(len(found.Changes), qt.Equals, 1)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `adding unique index "by_payload" to table "[^"]+\.records", which exists already, `+
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target; YDB keeps it behind feature flag EnableAddUniqueIndex: .*`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(fixture.heldIndexes(c), qt.HasLen, 0)
}
