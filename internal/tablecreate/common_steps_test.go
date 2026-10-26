package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// commonObjectStep is a host step that changes a common object.
func commonObjectStep(name string, change *protocol.CommonObjectChange) *protocol.CommonPlanningStep {
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: name}, CommonObject: change,
		Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common object"}}
}

func commentOn(kind, name string, path ...string) *protocol.CommonObjectChange {
	object := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: "app", Name: name, Path: path}
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_SetComment{SetComment: &protocol.ObjectComment{Object: object, Text: "note"}}}
}

// commentedTable gives a comment step the parent and the effect a host
// gives a step on app.records, so the step reads as one of the table's.
func commentedTable(c *qt.C, step *protocol.CommonPlanningStep) *protocol.CommonPlanningStep {
	c.Helper()
	subject := creationPlan(c).Tables[0].Subject
	step.Parent = proto.CloneOf(subject)
	step.Metadata.Effects = []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_ALTER}}
	return step
}

// commonCreation creates app.records on a host that negotiated the common
// wire, with the host's step for the table and the common steps given.
func commonCreation(c *qt.C, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := creationPlan(c)
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}
	subject := request.Tables[0].Subject
	request.CommonSteps = append([]*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: proto.CloneOf(subject),
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}}}, steps...)
	return request
}

// The host's view and comment steps stay the host's. A view is dropped before
// the table steps and created after them, and a comment on the table or on
// one of its columns comes after the table exists; a comment on another table
// is not ordered against it.
func TestPlanOrdersCommonObjectSteps(t *testing.T) {
	c := qt.New(t)
	view := &protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: "v"}
	request := commonCreation(c,
		commonObjectStep("drop-view", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropView{DropView: view}}),
		commonObjectStep("create-view", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
			View: &protocol.SQLView{Schema: "app", Name: "v", Body: "SELECT 1"}}}}),
		commentedTable(c, commonObjectStep("table-comment", commentOn("table", "records"))),
		commonObjectStep("column-comment", commentOn("column", "value", "records")),
		commonObjectStep("other-comment", commentOn("table", "other")))
	request.Tables[0].Desired.Comment = "rows"
	request.Tables[0].Desired.Columns[0].Comment = "the key"
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	c.Assert(plan.Rewrites[0].Sources[0].Name, qt.Equals, "create")
	var order [][2]string
	for _, dependency := range plan.Contributions[0].Dependencies {
		order = append(order, [2]string{dependency.Before.Name, dependency.After.Name})
	}
	created := plan.Contributions[0].Steps[0].Id.Name
	c.Assert(order, qt.DeepEquals, [][2]string{{"drop-view", created}, {created, "create-view"}, {created, "table-comment"}, {created, "column-comment"}})

	// The comments a table declares are the common wire's: the creation
	// carries none of them.
	rendered, err := backend.New().Render(c.Context(), testplanning.RenderRequest(plan, request.Capabilities))
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(string(rendered.Units[0].GetNative().GetPayload()), qt.Not(qt.Contains), "ptah.comment")
	c.Assert(string(rendered.Units[0].GetNative().GetPayload()), qt.Not(qt.Contains), "the key")
}

// On a host that did not negotiate the wire a declared comment is a table
// setting no creation states, and the creation is refused.
func TestPlanRefusesCommentsOffTheCommonWire(t *testing.T) {
	c := qt.New(t)
	request := commonCreation(c)
	request.TargetFeatures = nil
	request.Tables[0].Desired.Comment = "rows"
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `.*YDB table creation does not support .*DeclaredTable\.comment.*`)
}

// A comment YDB cannot keep refuses the plan before anything is planned.
func TestPlanRefusesACommentYDBCannotHold(t *testing.T) {
	c := qt.New(t)
	request := commonCreation(c)
	request.Tables[0].Desired.Columns[0].Comment = string([]byte{0xff})
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*the comment on column "first" of table app/records: the comment is not UTF-8 text.*`)
	c.Assert(plan, qt.IsNil)
}
