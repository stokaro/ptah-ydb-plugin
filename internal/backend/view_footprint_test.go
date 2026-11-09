package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

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
// beside the host's step on the view name and the steps after it.
func viewPlan(c *qt.C, action, name string, operation viewChange, after ...*protocol.CommonPlanningStep) (*protocol.PlannedFeatures, *protocol.CommonPlanningStep) {
	c.Helper()
	request := nativePlan(c, action)
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}
	view := commonView(name, operation)
	request.CommonSteps = append([]*protocol.CommonPlanningStep{view}, after...)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	return result, view
}

// A view occupies a path of its directory, as a table does, so a native
// object planned in the same run as a view at another path is not refused for
// the view's footprint, and needs no order against it: the node follows only
// the last common step.
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

			result, _ := viewPlan(c, test.action, "summary", test.operation, lastCommonStep())

			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(stepEdges(result), qt.DeepEquals, []string{placedLast})
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

// commonViewAt is commonView for the view the host names name in directory,
// which YDB keeps at the path directory/name.
func commonViewAt(directory, name string) *protocol.CommonPlanningStep {
	step := commonView(name, createView)
	step.CommonObject.GetCreateView().View.Schema = directory
	subject := step.Metadata.Effects[0].Subject
	subject.Schema = testcomparison.Part(directory)
	return step
}

// planTopicBesideView plans the creation of the topic app/events beside the
// host's creation of the view name in directory.
func planTopicBesideView(c *qt.C, directory, name string) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := topicPlanRequest(map[string]bool{"topics": true, "transactional_ddl": false}, []*protocol.CommonPlanningStep{commonViewAt(directory, name)},
		plannedTopic(c, "app", "events", "create"))
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return request, result
}

// A topic created at the path of a view the run creates is refused: one path
// holds one object. The host resolves how a view's name is spelled into its
// directory and name before it hands the step over. Ported from the host's
// TestYDBSharedPaths_RefusesAViewAtATopicsPath.
func TestTopicAtThePathOfACreatedView_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	_, result := planTopicBesideView(c, "app", "events")

	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Equals, "topic operation conflicts with a common occupant at its path")
	c.Assert(result.Contributions, qt.HasLen, 0)
}

// A view whose dotted name is one path segment at the root, and one whose
// name differs from the topic's only in case, are at paths of their own: YDB
// paths are case-sensitive, and a dot inside a name is not a separator. The
// topic plans beside either with no order against it. Ported from the host's
// TestYDBSharedPaths_KeepDistinctPaths.
func TestTopicBesideAViewAtAnotherPath_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, directory, view string }{
		{name: "a dotted name at the root", view: "app.events"},
		{name: "a name in another case", directory: "app", view: "Events"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			request, result := planTopicBesideView(c, test.directory, test.view)

			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(stepEdges(result), qt.HasLen, 0)
		})
	}
}

// A coordination node at app/locks plans beside a view whose dotted name
// "app.locks" is one path segment at the root: they are two paths. The node
// follows only the last common step, as it does beside any view. Ported from
// the host's TestValidateSchema_YDBTakesACoordinationNodeBesideOtherObjects.
func TestNodeBesideAViewWithADottedName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := nativePlan(c, "create")
	request.Changes = []*protocol.PlanningChange{createdNode(c, "node", "app", "locks")}
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}
	request.CommonSteps = []*protocol.CommonPlanningStep{commonViewAt("", "app.locks")}

	result, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	c.Assert(stepEdges(result), qt.DeepEquals, []string{"view -> coordination/000000"})
}
