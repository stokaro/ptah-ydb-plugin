package tablecreate_test

import (
	"maps"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// feedKnowledge is what a request says about the changefeeds of app.records.
type feedKnowledge struct {
	// desired and held are the changefeed sets of both sides; empty states
	// none.
	desired, held string
	// kind is what the read records about the changefeeds of every table, and
	// subject what it records about those of app.records alone, each with
	// reason; zero records nothing.
	kind, subject protocol.ModelKnowledge_State
	reason        string
}

// feedReadCoverage is the coverage of a read that records in about the
// changefeeds of app.records.
func feedReadCoverage(in feedKnowledge) *protocol.ComparisonCoverage {
	coverage := &protocol.ComparisonCoverage{}
	models := schema.ObservedModels()
	model := models[slices.IndexFunc(models, func(model *protocol.ValueModel) bool { return model.Kind == schema.ChangefeedProperties })]
	if in.kind != protocol.ModelKnowledge_STATE_UNSPECIFIED {
		coverage.Kinds = append(coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model,
			Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: in.kind, Reason: in.reason}}}})
	}
	if in.subject != protocol.ModelKnowledge_STATE_UNSPECIFIED {
		coverage.Subjects = append(coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: schema.ChangefeedProperties, Subject: tableSubject(),
			Knowledge: &protocol.ModelKnowledge{State: in.subject, Reason: in.reason}})
	}
	return coverage
}

// knowledgeRebuildRequest is rebuildRequest asked to rebuild, on a target with
// changefeeds, where both sides say in about the table's changefeeds.
func knowledgeRebuildRequest(c *qt.C, in feedKnowledge) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	maps.Copy(request.Capabilities, newest())
	table := request.Tables[0]
	if in.desired != "" {
		table.Desired.Features.Facets.Records = append(table.Desired.Features.Facets.Records, facet(c, schema.ChangefeedProperties, "", in.desired))
	}
	if in.held != "" {
		table.Current.Features.Facets.Records = append(table.Current.Features.Facets.Records, observedFacet(c, schema.ChangefeedProperties, "", in.held))
	}
	table.Current.Features.Facets.Coverage = feedReadCoverage(in)
	return request
}

// rebuildStepNames is the name of each step of a plan, in order.
func rebuildStepNames(plan *protocol.PlannedFeatures) []string {
	var names []string
	for _, contribution := range plan.Contributions {
		for _, step := range contribution.Steps {
			names = append(names, step.Id.Name)
		}
	}
	return names
}

// declaredHeldUpdates declares the changefeed updates that heldUpdates reads.
const declaredHeldUpdates = `{"changefeeds":[{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},` +
	`"consumers":[{"name":"audit","important":true}]}}]}`

// A rebuild drops every changefeed of the table before YDB moves it, so it
// needs a read that says which ones the table holds. A read whose coverage
// establishes that the table holds none, and a read that carries the table's
// changefeed set, are enough. The provider's own read records its knowledge
// as limited to the tables it returned and carries a set for each of them, so
// the set decides over that record. Ported from the host's
// TestGenerateMigrationAST_Changefeeds_RebuildRequiresCapturedKnowledge.
func TestRebuildPlanRequiresKnownChangefeeds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		in   feedKnowledge
	}{
		{name: "a read that records the table holds none", in: feedKnowledge{kind: protocol.ModelKnowledge_COMPLETE}},
		{name: "a read of the tables it returned, which carries an empty set", in: feedKnowledge{held: noFeeds,
			kind: protocol.ModelKnowledge_UNINSPECTED, reason: "Knowledge is limited to returned subjects and fully listed directories"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := knowledgeRebuildRequest(c, test.in)

			plan, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(rebuildStepNames(plan), qt.DeepEquals, []string{"table-rebuild/000000/create", "table-rebuild/000000/copy", "table-rebuild/000000/rename-old",
				"table-rebuild/000000/rename-new", "table-rebuild/000000/drop-old"})
		})
	}
}

// A rebuild is refused where the read cannot say which changefeeds the table
// holds, or holds one this provider cannot write again: the read captured
// none while the declaration states some, which the alteration refuses too;
// its coverage records them as uninspected, kind-wide or for the table, or as
// unrepresentable; or it carries a changefeed this provider cannot represent.
// Planned on such a read, the rename would fail after the new table is
// created and filled (`Cannot move table with cdc streams`). The host's
// operand-consistency rows, a captured table of another name, a changefeed as
// a facet rather than an object, and a child of another table, test its
// wiring and have no counterpart here. Ported from the host's
// TestGenerateMigrationAST_Changefeeds_RebuildRequiresCapturedKnowledge.
//
// Two documents that cannot describe changefeeds, such as two HCL files the
// host diffs, give a read the host records as uninspected for this model.
// The rebuild between them is refused rather than planned as if the table had
// none. Ported from the offline part of the host's
// TestSchemaDiffRefusesARebuildWithoutChangefeedKnowledge.
func TestRebuildPlanRequiresKnownChangefeeds_FailurePath(t *testing.T) {
	const hcl = `HCL describes this model only for a table that writes a platform "ydb" block`
	for _, test := range []struct {
		name string
		in   feedKnowledge
		want string
	}{
		{name: "changefeeds the read did not capture", in: feedKnowledge{desired: declaredHeldUpdates},
			want: `rebuilding table "app/records": the table's current changefeeds were not captured, and YDB moves no table that carries one ` +
				"(`Cannot move table with cdc streams`)"},
		{name: "a read that could not inspect them", in: feedKnowledge{desired: declaredHeldUpdates, kind: protocol.ModelKnowledge_UNINSPECTED, reason: "source format has no streams"},
			want: `rebuilding table "app/records": the changefeed namespace is not fully described: source format has no streams`},
		{name: "a read that records the table's changefeeds as unrepresentable", in: feedKnowledge{desired: declaredHeldUpdates,
			kind: protocol.ModelKnowledge_COMPLETE, subject: protocol.ModelKnowledge_UNREPRESENTABLE, reason: "unsupported settings"},
			want: `rebuilding table "app/records": the changefeed namespace is not fully described: unsupported settings`},
		{name: "a changefeed the read cannot represent", in: feedKnowledge{desired: declaredHeldUpdates,
			held: `{"changefeeds":[{"description":{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED","awsRegion":"eu-west-1"},` +
				`"topic":{"partitioningSettings":{}}}]}`},
			want: `rebuilding table "app/records": changefeed "updates" has settings this provider cannot represent, so the current changefeed was not established: ` +
				`it names an AWS region, which Ptah does not model`},
		{name: "two documents that cannot describe changefeeds", in: feedKnowledge{kind: protocol.ModelKnowledge_UNINSPECTED, reason: hcl},
			want: `rebuilding table "app/records": the changefeed namespace is not fully described: ` + hcl},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			plan, err := backend.New().PlanFeatures(c.Context(), knowledgeRebuildRequest(c, test.in))

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Code, qt.Equals, protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.want)
		})
	}
}
