package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
)

// tableAccessStep is the host's step that grants on the table or the view
// name of the directory app, which YDB grants on as on a table, or that
// comments on it, with the footprint the host gives it: a read of the object
// and an alteration of its access list, or of its comment, a resource of the
// kind resource named by the object.
func tableAccessStep(resource, name string) *protocol.CommonPlanningStep {
	target := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: name}
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "readers", Privileges: []string{"SELECT ROW"}, Target: target}}}
	if resource == "comment" {
		change.Operation = &protocol.CommonObjectChange_SetComment{SetComment: &protocol.ObjectComment{Object: target, Text: "Active ids"}}
	}
	object := &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"),
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
	held := &protocol.ComparisonIdentity{Kind: resource, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"),
		Path: []*protocol.ComparisonPart{testcomparison.Part("table")}, Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: resource}, CommonObject: change,
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Change " + resource,
			Effects: []*protocol.PlanningEffect{{Subject: object, Action: protocol.PlanningEffect_READ}, {Subject: held, Action: protocol.PlanningEffect_ALTER}}}}
}

// A grant on a table or a view, and a comment on a table, change a resource
// of the object's own and occupy no path, so a native object planned in the
// same run is not refused for the step's footprint and needs no order
// against it: the node follows only the last common step. The host's engine
// plans both in one run.
func TestTableAccessBesideANativeObject_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action, resource string
	}{
		{name: "a grant beside a creation", action: "create", resource: "access"},
		{name: "a grant beside an alteration", action: "alter", resource: "access"},
		{name: "a grant beside a drop", action: "drop", resource: "access"},
		{name: "a comment beside an alteration", action: "alter", resource: "comment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nativePlan(c, test.action)
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			request.CommonSteps = []*protocol.CommonPlanningStep{tableAccessStep(test.resource, "active"), lastCommonStep()}

			result, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(stepEdges(result), qt.DeepEquals, []string{placedLast})
		})
	}
}

// A step that names a resource of its own at the node's path still reads the
// object there, which is a table where the node is created: the path is
// taken, and the creation is refused.
func TestTableAccessAtTheNodesPath_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := nativePlan(c, "create")
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
	request.CommonSteps = []*protocol.CommonPlanningStep{tableAccessStep("access", "node")}

	result, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Contributions, qt.HasLen, 0)
}
