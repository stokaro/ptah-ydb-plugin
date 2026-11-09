package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// lastCommonStep is a host step after every other one of a test that needs
// nothing of a node: the creation of the table elsewhere/last. A node's steps
// follow the last common step that needs none of them, so a test that
// appends it reads every other edge as an order the steps ahead of it impose.
func lastCommonStep() *protocol.CommonPlanningStep {
	return commonAt("table", "elsewhere", "last", protocol.PlanningEffect_CREATE)
}

// placedLast is the edge from lastCommonStep to the step of the only node a
// test plans.
const placedLast = "create table elsewhere/last -> coordination/000000"

// commonRoleDrop is the host's step that drops the role app.
func commonRoleDrop() *protocol.CommonPlanningStep {
	role := &protocol.ComparisonIdentity{Kind: "role", Name: &protocol.ComparisonPart{Source: "app", Normalized: "app"}}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "drop role app"},
		CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropRole{DropRole: &protocol.Reference{Engine: "ydb", Kind: "role", Name: "app"}}},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Drop a role",
			Effects: []*protocol.PlanningEffect{{Subject: role, Action: protocol.PlanningEffect_DROP}}}}
}

// commonSteps are the host steps a row of
// TestCoordinationNodeFollowsTheCommonSteps_HappyPath places around the node
// of a request, by name: a grant on the node, the creation of a table at the
// node's path, the drop of a role, the drop of the table app/t, and the
// creation of the tables app/t and app/u.
var commonSteps = map[string]func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep{
	"grant": func(request *protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonNativeAccess(request, grantAccess)
	},
	"path": func(request *protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonPath(request, protocol.PlanningEffect_CREATE)
	},
	"drop role": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep { return commonRoleDrop() },
	"drop t": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAt("table", "app", "t", protocol.PlanningEffect_DROP)
	},
	"t": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAt("table", "app", "t", protocol.PlanningEffect_CREATE)
	},
	"u": func(*protocol.PlanFeaturesRequest) *protocol.CommonPlanningStep {
		return commonAt("table", "app", "u", protocol.PlanningEffect_CREATE)
	},
}

// The YDB engine built into the host at 74de0b65b ran a node's statement once
// its common statements could go no further without the node: right before
// the first common step that needs it, or after all of them. The provider's
// owner sorts ahead of the host's, so without an order of its own a node's
// step ran ahead of every common step: `schema apply` created the node ahead
// of the group and the table, and dropped it ahead of the table.
func TestCoordinationNodeFollowsTheCommonSteps_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action string
		common       []string
		edges        []string
	}{
		{name: "a creation follows the steps ahead of the grant on it", action: "create", common: []string{"t", "grant", "u"},
			edges: []string{"coordination/000000 -> access", "create table app/t -> coordination/000000"}},
		{name: "a creation nothing needs follows the last step", action: "create", common: []string{"t", "u"},
			edges: []string{"create table app/u -> coordination/000000"}},
		{name: "an alteration follows the last step", action: "alter", common: []string{"t", "u"},
			edges: []string{"create table app/u -> coordination/000000"}},
		{name: "a drop follows the last step", action: "drop", common: []string{"drop t"},
			edges: []string{"drop table app/t -> coordination/000000"}},
		{name: "a drop precedes the drop of a role", action: "drop", common: []string{"drop t", "drop role"},
			edges: []string{"coordination/000000 -> drop role app", "drop table app/t -> coordination/000000"}},
		{name: "a drop precedes a creation at its path", action: "drop", common: []string{"t", "path"},
			edges: []string{"coordination/000000 -> table", "create table app/t -> coordination/000000"}},
		{name: "a drop the first step needs follows nothing", action: "drop", common: []string{"path", "u"},
			edges: []string{"coordination/000000 -> table"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nativePlan(c, test.action)
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			for _, name := range test.common {
				request.CommonSteps = append(request.CommonSteps, commonSteps[name](request))
			}

			result, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(stepEdges(result), qt.DeepEquals, test.edges)
		})
	}
}
