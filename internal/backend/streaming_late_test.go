package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// streamingCommonSteps are the host steps a row of
// TestStreamingQueryFollowsTheCommonSteps_HappyPath places around the query
// of a request, by name: a grant on the query, the drop of a role, and the
// creation of the tables app/t and app/u.
var streamingCommonSteps = map[string]func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep{
	"grant": func(request *protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAccessFor(request.Changes[0].Subject, "streaming query", grantAccess)
	},
	"drop role": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep { return commonRoleDrop() },
	"t": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAt("table", "app", "t", protocol.PlanningEffect_CREATE)
	},
	"u": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAt("table", "app", "u", protocol.PlanningEffect_CREATE)
	},
}

// The YDB engine built into the host at 74de0b65b created, changed or started
// a streaming query after every statement of the host, and granted on it
// last. A step that creates, changes or restarts a query follows the last
// common step: the grant on the query follows the step, and the common steps
// after the grant do not wait for it, so the step follows them too, but a
// principal change after the grant waits for it, so the step goes ahead of
// that change. A stop or a drop runs early and follows nothing. The
// provider's owner sorts ahead of the host's, so without an order of its own
// `schema apply` created the query ahead of the group, the tables and the
// grants.
func TestStreamingQueryFollowsTheCommonSteps_HappyPath(t *testing.T) {
	body := &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/source`;"}
	changed := &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/next`;"}
	stopped := &streaming.Spec{Text: body.Text, Run: new(false)}
	for _, test := range []struct {
		name          string
		before, after *streaming.Spec
		common        []string
		edges         []string
	}{
		{name: "a creation follows the steps after the grant on it", after: body, common: []string{"t", "grant", "u"},
			edges: []string{"streaming/000000/0 -> access", "create table app/u -> streaming/000000/0"}},
		{name: "a creation goes ahead of a principal change after the grant on it", after: body, common: []string{"t", "grant", "drop role", "u"},
			edges: []string{"streaming/000000/0 -> access", "create table app/t -> streaming/000000/0"}},
		{name: "a change of a stopped query follows the last step", before: stopped, after: &streaming.Spec{Text: changed.Text, Run: new(false)}, common: []string{"t", "u"},
			edges: []string{"create table app/u -> streaming/000000/0"}},
		{name: "a restart follows the last step and its stop nothing", before: body, after: changed, common: []string{"t", "u"},
			edges: []string{"streaming/000000/0 -> streaming/000000/1", "create table app/u -> streaming/000000/1"}},
		{name: "a drop follows nothing", before: body, common: []string{"t", "u"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := streamingPlanRequest(c, test.before, test.after, true)
			request.TargetFeatures = append(request.TargetFeatures, provider.CommonObjectsFeature, provider.CommonAccessFeature)
			for _, name := range test.common {
				request.CommonSteps = append(request.CommonSteps, streamingCommonSteps[name](request))
			}

			result, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(stepEdges(result), qt.DeepEquals, test.edges)
		})
	}
}
