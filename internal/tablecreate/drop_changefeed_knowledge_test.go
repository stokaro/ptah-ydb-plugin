package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// knowledgeDropRequest is dropRequest on a read that says in about the
// changefeeds of app.records.
func knowledgeDropRequest(c *qt.C, in feedKnowledge) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := dropRequest(c)
	current := request.Tables[0].Current
	if in.held != "" {
		current.Features.Facets.Records = append(current.Features.Facets.Records, observedFacet(c, schema.ChangefeedProperties, "", in.held))
	}
	current.Features.Facets.Coverage = feedReadCoverage(in)
	return request
}

// A table drops with its changefeeds where the read says which ones it holds:
// none, a set the read carries, or one stream of it. Ported from the host's
// TestPlannerDropsKnownChildrenWithTheirParent, beside
// TestPlannerRefusesDropWithUnknownCapturedChildren.
func TestDropPlanRequiresKnownChangefeeds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		in   feedKnowledge
	}{
		{name: "a read that records the table holds none", in: feedKnowledge{kind: protocol.ModelKnowledge_COMPLETE}},
		{name: "a read of the tables it returned, which carries an empty set", in: feedKnowledge{held: noFeeds,
			kind: protocol.ModelKnowledge_UNINSPECTED, reason: "Knowledge is limited to returned subjects and fully listed directories"}},
		{name: "a read that carries a changefeed", in: feedKnowledge{held: heldUpdates, kind: protocol.ModelKnowledge_COMPLETE}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := knowledgeDropRequest(c, test.in)

			plan, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(renderedText(c, plan, request.Capabilities), qt.DeepEquals, []string{"DROP TABLE `app/records`;"})
		})
	}
}

// A drop is refused where the read records the table's changefeeds as
// uninspected or unrepresentable and carries none of them, kind-wide or for
// the table: DROP TABLE takes every changefeed with it, and the plan cannot
// tell whether a replication reads one. The refusal gives the read's reason.
// Ported from the host's TestPlannerRefusesDropWithUnknownCapturedChildren:
// its rows for one changefeed recorded as unknown have no counterpart, since
// the read records changefeeds per table, and carries a changefeed it cannot
// represent in the table's set.
func TestDropPlanRequiresKnownChangefeeds_FailurePath(t *testing.T) {
	const reason = "source cannot represent stream ownership"
	for _, test := range []struct {
		name string
		in   feedKnowledge
	}{
		{name: "changefeeds uninspected kind-wide", in: feedKnowledge{kind: protocol.ModelKnowledge_UNINSPECTED, reason: reason}},
		{name: "changefeeds unrepresentable kind-wide", in: feedKnowledge{kind: protocol.ModelKnowledge_UNREPRESENTABLE, reason: reason}},
		{name: "the table's changefeeds uninspected", in: feedKnowledge{kind: protocol.ModelKnowledge_COMPLETE, subject: protocol.ModelKnowledge_UNINSPECTED, reason: reason}},
		{name: "the table's changefeeds unrepresentable", in: feedKnowledge{kind: protocol.ModelKnowledge_COMPLETE, subject: protocol.ModelKnowledge_UNREPRESENTABLE, reason: reason}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			plan, err := backend.New().PlanFeatures(c.Context(), knowledgeDropRequest(c, test.in))

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Code, qt.Equals, protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, `dropping table "app/records": the changefeed namespace is not fully described: `+reason)
		})
	}
}
