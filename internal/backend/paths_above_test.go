package backend_test

import (
	"fmt"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The tests here plan coordination nodes beside objects at the paths above
// and below them. YDB needs every path above an object to be a directory, so
// a creation below a path follows the drop of the object there, and a drop
// precedes a creation below its path.

// nodeOwner is the coordination node at directory/name of the fixture
// database.
func nodeOwner(directory, name string) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: directory, Name: name}
}

// createdNode is the change that creates the node at directory/name, under
// the value identity id.
func createdNode(c *qt.C, id, directory, name string) *protocol.PlanningChange {
	c.Helper()
	owner := nodeOwner(directory, name)
	desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureCoordinationChange(id, owner, nil, desired)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: testplanning.Subject(owner), Value: change}
}

// droppedNode is the change that drops the node at directory/name, read with
// the settings coordinationObserved gives, under the value identity id.
func droppedNode(c *qt.C, id, directory, name string) *protocol.PlanningChange {
	c.Helper()
	owner := nodeOwner(directory, name)
	observed, err := schema.CaptureObject(owner, coordinationObserved())
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureCoordinationChange(id, owner, observed, nil)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: testplanning.Subject(owner), Value: change}
}

// commonAt is a host step that acts with action on the object of kind at
// directory/name, as a table's or a view's step reports its footprint. Its
// name says what it does.
func commonAt(kind, directory, name string, action protocol.PlanningEffect_Action) *protocol.CommonPlanningStep {
	subject := &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part("/local"), Schema: testcomparison.Part(directory), Name: testcomparison.Part(name)}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: strings.ToLower(action.String()) + " " + kind + " " + directory + "/" + name}, Metadata: &protocol.PlanningMetadata{
		Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation",
		Effects: []*protocol.PlanningEffect{{Subject: subject, Action: action}}}}
}

// nodeChanges are the two ways a row names a node change.
var nodeChanges = map[string]func(c *qt.C, id, directory, name string) *protocol.PlanningChange{"create": createdNode, "drop": droppedNode}

// planNodes plans changes, each a node change of the coordination service,
// beside the host's steps common.
func planNodes(c *qt.C, common []*protocol.CommonPlanningStep, changes ...*protocol.PlanningChange) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := nativePlan(c, "create")
	request.Changes = changes
	request.CommonSteps = common
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return request, result
}

// stepOf is the step a plan carries out the change of the node at
// directory/name with.
func stepOf(c *qt.C, result *protocol.PlannedFeatures, directory, name string) *protocol.StepIdentity {
	c.Helper()
	for _, receipt := range result.GetChanges() {
		if receipt.GetSubject().GetSchema().GetNormalized() == directory && receipt.GetSubject().GetName().GetNormalized() == name {
			return receipt.GetSteps()[0]
		}
	}
	c.Fatalf("no step for %s/%s", directory, name)
	return nil
}

// stepEdges writes a plan's dependencies, each as the names of its two steps.
func stepEdges(result *protocol.PlannedFeatures) []string {
	var written []string
	for _, contribution := range result.GetContributions() {
		for _, dependency := range contribution.GetDependencies() {
			written = append(written, dependency.GetBefore().GetName()+" -> "+dependency.GetAfter().GetName())
		}
	}
	return written
}

// directoryEffects writes the effects of a plan's steps on directories, each
// as its step, the directory and the action.
func directoryEffects(result *protocol.PlannedFeatures) []string {
	var written []string
	for _, contribution := range result.GetContributions() {
		for _, step := range contribution.GetSteps() {
			for _, effect := range step.GetMetadata().GetEffects() {
				if effect.GetSubject().GetKind() == "ydb.scheme-directory" {
					written = append(written, fmt.Sprintf("%s %s|%s %s", step.GetId().GetName(), effect.GetSubject().GetSchema().GetNormalized(),
						effect.GetSubject().GetName().GetNormalized(), effect.GetAction()))
				}
			}
		}
	}
	return written
}

// A node created below the path of a table the plan drops follows the drop,
// and so does one below a path the plan drops after a step that still reads
// the table there. A node dropped above a path where the plan creates a view
// precedes the view.
func TestNodeOrdersAgainstCommonStepsAboveAndBelow_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action, directory string
		common                  []*protocol.CommonPlanningStep
		want                    []string
	}{
		{name: "a creation below a dropped table", action: "create", directory: "app/holder",
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "holder", protocol.PlanningEffect_DROP)},
			want:   []string{"drop table app/holder -> coordination/000000"}},
		{name: "a creation two levels below a dropped table", action: "create", directory: "app/holder/deep",
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "holder", protocol.PlanningEffect_DROP)},
			want:   []string{"drop table app/holder -> coordination/000000"}},
		{name: "a creation below a table read before its drop", action: "create", directory: "app/holder",
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "holder", protocol.PlanningEffect_READ),
				commonAt("table", "app", "holder", protocol.PlanningEffect_DROP)},
			want: []string{"drop table app/holder -> coordination/000000"}},
		{name: "a drop above a created view", action: "drop", directory: "app",
			common: []*protocol.CommonPlanningStep{commonAt("view", "app/lock", "summary", protocol.PlanningEffect_CREATE)},
			want:   []string{"coordination/000000 -> create view app/lock/summary"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			request, result := planNodes(c, test.common, nodeChanges[test.action](c, "node", test.directory, "lock"))

			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(stepEdges(result), qt.DeepEquals, test.want)
		})
	}
}

// A node created below the path of a node the same plan drops follows that
// drop. The drop records the directory it leaves room for, and the creation
// reads each directory above it, which orders the two the same way when
// another planning service plans one of them.
func TestNodeBelowADroppedNodeFollowsTheDrop_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	request, result := planNodes(c, nil, createdNode(c, "below", "app/occupied", "lock"), droppedNode(c, "above", "app", "occupied"))

	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	dropped, created := stepOf(c, result, "app", "occupied"), stepOf(c, result, "app/occupied", "lock")
	c.Assert(stepEdges(result), qt.DeepEquals, []string{dropped.GetName() + " -> " + created.GetName()})
	c.Assert(directoryEffects(result), qt.DeepEquals, []string{
		dropped.GetName() + " app|occupied CREATE",
		created.GetName() + " |app READ",
		created.GetName() + " app|occupied READ",
	})
}

// No order of the plan's steps can leave a directory where it must be: a node
// created below a table the plan keeps, a node dropped below a path where the
// plan creates a table (YDB keeps the directory after its last object goes,
// and YQL has no statement that removes one), and a node created where the
// plan drops an object below.
func TestNodeAgainstCommonStepsAboveAndBelow_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, action, directory, message string
		common                           *protocol.CommonPlanningStep
	}{
		{name: "a creation below a kept table", action: "create", directory: "app/holder",
			common:  commonAt("table", "app", "holder", protocol.PlanningEffect_READ),
			message: `overlapping parent and descendant paths require a captured directory lifecycle`},
		{name: "a drop below a created table", action: "drop", directory: "app/holder",
			common:  commonAt("table", "app", "holder", protocol.PlanningEffect_CREATE),
			message: "this plan drops app/holder/lock and creates an object at app/holder, which is a directory in the database. .*"},
		{name: "a creation above a dropped view", action: "create", directory: "app",
			common:  commonAt("view", "app/lock", "summary", protocol.PlanningEffect_DROP),
			message: "this plan drops app/lock/summary and creates an object at app/lock, which is a directory in the database. .*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			_, result := planNodes(c, []*protocol.CommonPlanningStep{test.common}, nodeChanges[test.action](c, "node", test.directory, "lock"))

			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, test.message)
			c.Assert(result.Contributions, qt.HasLen, 0)
		})
	}
}

// A node created at a path where the same plan drops a node below is refused
// as the host's drop below is: YDB keeps the directory there.
func TestNodeAboveADroppedNode_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	_, result := planNodes(c, nil, createdNode(c, "above", "app", "occupied"), droppedNode(c, "below", "app/occupied", "lock"))

	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, "this plan drops app/occupied/lock and creates an object at app/occupied, which is a directory in the database. .*")
	c.Assert(result.Contributions, qt.HasLen, 0)
}

// planStreaming plans the change of the streaming query at directory/copy
// from before to after beside the host's steps common.
func planStreaming(c *qt.C, directory string, before, after *streaming.Spec, common []*protocol.CommonPlanningStep) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := streamingPlanRequestAt(c, directory, "copy", before, after, false)
	request.CommonSteps = common
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return request, result
}

// A streaming query created below two paths the plan drops follows both
// drops, since YDB needs every path above an object to be a directory, as a
// coordination node does. Ported from the host's
// TestStandalonePlan_CreationFollowsADropAbove.
func TestStreamingQueryBelowDroppedPathsFollowsTheDrops_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	common := []*protocol.CommonPlanningStep{commonAt("table", "", "app", protocol.PlanningEffect_DROP), commonAt("table", "app", "ext", protocol.PlanningEffect_DROP)}

	request, result := planStreaming(c, "app/ext", nil, &streaming.Spec{Text: "SELECT 1;"}, common)

	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	created := result.Changes[0].Steps[0].GetName()
	c.Assert(stepEdges(result), qt.DeepEquals, []string{"drop table /app -> " + created, "drop table app/ext -> " + created})
}

// A streaming query dropped below two paths the plan creates objects at is
// refused, as a coordination node's drop is: YDB keeps the directory after
// its last object goes, so no order lets the creations run. The host's
// embedded engine ordered the drop first and the creations after it, which
// YDB then refused. Ported from the host's
// TestStandalonePlan_DropPrecedesACreationAbove.
func TestStreamingQueryDroppedBelowCreatedPaths_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	common := []*protocol.CommonPlanningStep{commonAt("table", "", "app", protocol.PlanningEffect_CREATE), commonAt("table", "app", "ext", protocol.PlanningEffect_CREATE)}

	_, result := planStreaming(c, "app/ext", &streaming.Spec{Text: "SELECT 1;"}, nil, common)

	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, "this plan drops app/ext/copy and creates an object at app(/ext)?, which is a directory in the database. .*")
	c.Assert(result.Contributions, qt.HasLen, 0)
}
