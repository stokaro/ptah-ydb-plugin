package tablecreate_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func creationPlan(c *qt.C) *protocol.PlanFeaturesRequest {
	c.Helper()
	table := withSettings(c)
	table.Features.Objects = &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}
	table.Features.Facets.Coverage = &protocol.ComparisonCoverage{}
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, DatabasePath: "/local", ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false},
		Tables: []*protocol.PlanningTable{{Subject: &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("records")},
			Action: protocol.PlanningTable_CREATE, Desired: table}}}
}
func TestNativeTableCreationPlanRenderNormalize(t *testing.T) {
	c := qt.New(t)
	service := backend.New()
	request := creationPlan(c)
	subject := request.Tables[0].Subject
	commonID := &protocol.StepIdentity{Owner: "host", Name: "create"}
	request.CommonSteps = []*protocol.CommonPlanningStep{{Id: commonID, Parent: proto.CloneOf(subject),
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}}}
	original := proto.CloneOf(request)
	plan, err := service.PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Parents, qt.HasLen, 5)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	c.Assert(proto.Equal(plan.Rewrites[0].Sources[0], commonID), qt.IsTrue)
	step := plan.Contributions[0].Steps[0]
	c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
	c.Assert(step.Metadata.Effects, qt.HasLen, 2)
	c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.scheme-path")
	c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.OperationKind)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
	normalized, err := service.NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendered, err := service.Render(t.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(proto.Equal(rendered.Units[0].GetNative(), step.Operation.Value.Value), qt.IsTrue)
	c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
	rendering.Changes[0].GetNative().Value.OperationSemantics.Impact = protocol.ChangeSemantics_DESTRUCTIVE
	refused, err := service.Render(t.Context(), rendering)
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
}
func TestTableCreationSkipsOrderingMarkers(t *testing.T) {
	c := qt.New(t)
	request := creationPlan(c)
	subject := request.Tables[0].Subject
	commonID := &protocol.StepIdentity{Owner: "host", Name: "create"}
	marker := func(name string) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: name},
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Ordering marker"}}
	}
	request.CommonSteps = []*protocol.CommonPlanningStep{marker("common/prerequisite-creations"),
		{Id: commonID, Parent: proto.CloneOf(subject),
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}},
		marker("common/table-creations-end")}

	plan, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	c.Assert(proto.Equal(plan.Rewrites[0].Sources[0], commonID), qt.IsTrue)
}
func TestTableCreationRefusesParentlessStepWithEffects(t *testing.T) {
	c := qt.New(t)
	request := creationPlan(c)
	subject := request.Tables[0].Subject
	request.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "other"},
		Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation",
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ}}}}}

	plan, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, "A common step is outside the supported table creation lifecycle")
}
func TestTableCreationRefusesPartialPlanning(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*protocol.PlanFeaturesRequest)
	}{
		{"unmeasured capabilities", func(r *protocol.PlanFeaturesRequest) { r.Capabilities = nil }},
		{"transactional claim", func(r *protocol.PlanFeaturesRequest) { r.Capabilities["transactional_ddl"] = true }},
		{"unimplemented declaration", func(r *protocol.PlanFeaturesRequest) { r.Tables[0].Desired.Comment = "preserve" }},
		{"partial common footprint", func(r *protocol.PlanFeaturesRequest) {
			subject := r.Tables[0].Subject
			r.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "read"}, Parent: proto.CloneOf(subject),
				Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation",
					Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ}}}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := creationPlan(c)
			test.modify(request)
			result, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Parents, qt.HasLen, 0)
			c.Assert(result.Rewrites, qt.HasLen, 0)
		})
	}
	c := qt.New(t)
	request := creationPlan(c)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := backend.New().PlanFeatures(ctx, request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}
func TestCreationOperationRefusesSessionAndPathInjection(t *testing.T) {
	for _, payload := range []string{
		`{"sessionId":"smuggled"}`, `{"path":"/other/table"}`, `{"columns":null}`, `{"unknown":true}`,
		`{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":false}],"primaryKey":["id"],"storeType":"STORE_TYPE_ROW"}`,
		`{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"storeType":"STORE_TYPE_COLUMN"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c := qt.New(t)
			value, err := tablecreate.Capture("operation", owner(), declaration())
			c.Assert(err, qt.IsNil)
			value.Value.Payload = []byte(payload)
			normalized, err := tablecreate.Normalize(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: value.Value})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
