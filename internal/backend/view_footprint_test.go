package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
)

// commonView is the host's step that creates the view name in the directory
// app, or drops it, with the footprint the host gives it: the view's creation
// or its drop.
// viewChange is what the host's step does to a view.
type viewChange int

const (
	// createView creates the view.
	createView viewChange = iota
	// dropView drops the view.
	dropView
)

func commonView(name string, operation viewChange) *protocol.CommonPlanningStep {
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
		View: &protocol.SQLView{Schema: "app", Name: name, Body: "SELECT 1 AS id"}}}}
	action := protocol.PlanningEffect_CREATE
	if operation == dropView {
		change.Operation = &protocol.CommonObjectChange_DropView{DropView: &protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: name}}
		action = protocol.PlanningEffect_DROP
	}
	subject := &protocol.ComparisonIdentity{Kind: "view", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"),
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "view"}, CommonObject: change,
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Change a view",
			Effects: []*protocol.PlanningEffect{{Subject: subject, Action: action}}}}
}

// viewPlan plans the coordination node app/node, created as action says,
// beside the host's step on the view name.
func viewPlan(c *qt.C, action, name string, operation viewChange) (*protocol.PlannedFeatures, *protocol.CommonPlanningStep) {
	c.Helper()
	request := nativePlan(c, action)
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}
	view := commonView(name, operation)
	request.CommonSteps = []*protocol.CommonPlanningStep{view}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	return result, view
}

// A view occupies a path of its directory, as a table does, so a native
// object planned in the same run as a view at another path is not refused for
// the view's footprint, and needs no order against it.
func TestViewBesideANativeObject_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action string
		operation    viewChange
	}{
		{name: "a view created beside a creation", action: "create"},
		{name: "a view created beside an alteration", action: "alter"},
		{name: "a view dropped beside a drop", action: "drop", operation: dropView},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, _ := viewPlan(c, test.action, "summary", test.operation)

			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(result.Contributions[0].Dependencies, qt.HasLen, 0)
		})
	}
}

// A node created at the path of a view the run drops takes the path once the
// view has left it: the drop comes first.
func TestNodeTakesThePathOfADroppedView_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	result, view := viewPlan(c, "create", "node", dropView)

	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Contributions, qt.HasLen, 1)
	step := result.Contributions[0].Steps[0]
	c.Assert(result.Contributions[0].Dependencies, qt.HasLen, 1)
	c.Assert(proto.Equal(result.Contributions[0].Dependencies[0], &protocol.PlanningDependency{Before: view.Id, After: step.Id}), qt.IsTrue)
}

// A node created at the path of a view the run creates too is refused: one
// path holds one object.
func TestNodeAtThePathOfACreatedView_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	result, _ := viewPlan(c, "create", "node", createView)

	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, `.*conflicts with a common occupant at its path.*`)
	c.Assert(result.Contributions, qt.HasLen, 0)
}
