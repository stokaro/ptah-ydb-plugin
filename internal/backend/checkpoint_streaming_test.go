package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// A checkpoint compares a declared schema with a completely known empty
// baseline, then reverses its changes to write the down file. Port the
// streaming-query half of the host's
// TestGenerateCheckpoint_YDBStandaloneFeaturesUseSelectedOwners through the
// provider calls made by that path. The coordination-node half is exercised
// by the host checkpoint CLI in TestHostCheckpointStandaloneCoordinationUsesSelectedOwner.
func TestCheckpointEmptyBaselineSelectsStreamingQuery(t *testing.T) {
	c := testcheck.New(t)
	spec := streaming.Spec{Text: "SELECT 1;", Run: new(false)}
	comparison := streamingComparison()
	addObject(c, comparison.Desired, declaredQuery(c, "", "query", spec))
	c.Assert(comparison.Current.Records, qt.HasLen, 0)
	c.Assert(comparison.Current.Coverage.Kinds[0].Sources[0].Outside.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)

	change, _, reversed := reverseOf(c, comparison)
	c.Assert(change.Subject.Name.Normalized, qt.Equals, "query")
	c.Assert(reversed.Subject.Name.Normalized, qt.Equals, "query")

	for _, side := range []struct {
		name, statement string
		before, after   *streaming.Spec
		change          *protocol.PlanningChange
	}{
		{name: "up", statement: "CREATE STREAMING QUERY `query`", after: &spec,
			change: &protocol.PlanningChange{Subject: change.Subject, Value: change.Value}},
		{name: "down", statement: "DROP STREAMING QUERY `query`", before: &spec,
			change: &protocol.PlanningChange{Subject: reversed.Subject, Value: reversed.Value}},
	} {
		t.Run(side.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := streamingPlanRequestAt(c, "", "query", side.before, side.after, false)
			request.Changes = []*protocol.PlanningChange{side.change}
			planned, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			rendering := testplanning.RenderRequest(backend.Description(), planned, request.Capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendering.TargetFeatures = []string{provider.TextUnitsFeature}
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(rendered.Units[0].GetSql(), qt.Contains, side.statement)
		})
	}
}
