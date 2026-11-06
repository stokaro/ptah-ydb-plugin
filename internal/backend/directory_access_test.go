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

// commonDirectoryAccess is the host's step that grants, or revokes, LIST on
// the directory, with the footprint the host gives it: an alteration of the
// directory's access, named as a schema.
func commonDirectoryAccess(directory string, verb accessVerb) *protocol.CommonPlanningStep {
	grant := &protocol.SQLGrant{Role: "readers", Privileges: []string{"LIST"}, Target: &protocol.Reference{Engine: "ydb", Kind: "schema", Name: directory}}
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: grant}}
	if verb == revokeAccess {
		change.Operation = &protocol.CommonObjectChange_Revoke{Revoke: grant}
	}
	subject := &protocol.ComparisonIdentity{Kind: "schema", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{},
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(directory)}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "directory-access"}, CommonObject: change,
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Change directory access",
			Effects: []*protocol.PlanningEffect{{Subject: subject, Action: protocol.PlanningEffect_ALTER}}}}
}

// directoryPlan plans a coordination node at app/node beside a host step on
// the access of directory and the steps after it.
func directoryPlan(c *qt.C, action, directory string, verb accessVerb, after ...*protocol.CommonPlanningStep) (*protocol.PlannedFeatures, *protocol.CommonPlanningStep) {
	c.Helper()
	request := nativePlan(c, action)
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
	access := commonDirectoryAccess(directory, verb)
	request.CommonSteps = append([]*protocol.CommonPlanningStep{access}, after...)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	return result, access
}

// A grant or a revoke on a directory changes the directory's access list and
// occupies no path, so a native object planned beside it is not refused for
// the step's footprint. A creation in the directory, or below it, comes
// first: creating the object creates the directory where it is missing, and
// YDB grants nothing on a path that holds nothing.
func TestDirectoryAccessFollowsACreationBelowIt_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, directory string
		verb            accessVerb
	}{
		{name: "a grant on the directory", directory: "app"},
		{name: "a revoke on the directory", directory: "app", verb: revokeAccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, access := directoryPlan(c, "create", test.directory, test.verb)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 1)
			step := result.Contributions[0].Steps[0]
			c.Assert(result.Contributions[0].Dependencies, qt.HasLen, 1)
			c.Assert(proto.Equal(result.Contributions[0].Dependencies[0], &protocol.PlanningDependency{Before: step.Id, After: access.Id}), qt.IsTrue)
		})
	}
}

// A directory's access needs no order against an object outside it, nor
// against an alteration or a drop of one in it: the node follows only the
// last common step.
func TestDirectoryAccessLeavesOtherOperationsUnordered_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action, directory string
	}{
		{name: "a creation in another directory", action: "create", directory: "other"},
		{name: "a creation in a directory that shares a prefix", action: "create", directory: "ap"},
		{name: "an alteration in the directory", action: "alter", directory: "app"},
		{name: "a drop in the directory", action: "drop", directory: "app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, _ := directoryPlan(c, test.action, test.directory, grantAccess, lastCommonStep())
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(stepEdges(result), qt.DeepEquals, []string{placedLast})
		})
	}
}

// A directory access step whose footprint differs from its operand is
// refused rather than set aside.
func TestDirectoryAccessWithAForgedFootprint_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		action protocol.PlanningEffect_Action
		effect string
	}{
		{name: "another directory's access", action: protocol.PlanningEffect_ALTER, effect: "other"},
		{name: "a creation of the directory", action: protocol.PlanningEffect_CREATE, effect: "app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nativePlan(c, "create")
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			access := commonDirectoryAccess("app", grantAccess)
			access.Metadata.Effects[0].Action = test.action
			access.Metadata.Effects[0].Subject.Name = testcomparison.Part(test.effect)
			request.CommonSteps = []*protocol.CommonPlanningStep{access}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
		})
	}
}
