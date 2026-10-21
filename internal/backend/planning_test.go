package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func nativePlan(c *qt.C, action string) *protocol.PlanFeaturesRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
	desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	c.Assert(err, qt.IsNil)
	observed, err := schema.CaptureObject(owner, coordinationObserved())
	c.Assert(err, qt.IsNil)
	switch action {
	case "create":
		observed = nil
	case "drop":
		desired = nil
	}
	request, err := testplanning.Request(owner, observed, desired)
	c.Assert(err, qt.IsNil)
	return request
}

func TestCoordinationPlanningAndNativeRendering(t *testing.T) {
	for _, action := range []string{"create", "alter", "drop"} {
		t.Run(action, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, action)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Changes, qt.HasLen, 1)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, coordinationplan.PathKind)
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, schema.CoordinationOperation)
			rendering := testplanning.RenderRequest(plan, request.Capabilities)
			rendered, err := backend.New().Render(t.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(rendered.Units[0].GetSql(), qt.Equals, "")
			c.Assert(proto.Equal(rendered.Units[0].GetNative(), step.Operation.Value.Value), qt.IsTrue)
			c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
			// Re-normalization derives operation safety without trusting stored metadata.
			normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
			rendering.Changes[0].GetNative().Value.OperationSemantics.Access = protocol.ChangeSemantics_WIDENS
			refused, err := backend.New().Render(t.Context(), rendering)
			c.Assert(err, qt.IsNotNil)
			c.Assert(refused, qt.IsNil)
		})
	}
}

func commonPath(request *protocol.PlanFeaturesRequest, action protocol.PlanningEffect_Action) *protocol.CommonPlanningStep {
	subject := proto.CloneOf(request.Changes[0].Subject)
	subject.Kind = "table"
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "table"}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{{Subject: subject, Action: action}}}}
}

func TestCoordinationPlanningOrdersSharedPaths(t *testing.T) {
	for _, test := range []struct {
		name, action string
		common       protocol.PlanningEffect_Action
		refused      bool
		before       string
	}{
		{"replace table", "create", protocol.PlanningEffect_DROP, false, "host"},
		{"replace node", "drop", protocol.PlanningEffect_CREATE, false, schema.ProviderID},
		{"double create", "create", protocol.PlanningEffect_CREATE, true, ""},
		{"double drop", "drop", protocol.PlanningEffect_DROP, true, ""},
		{"alter occupied path", "alter", protocol.PlanningEffect_DROP, true, ""},
		{"unordered reader", "create", protocol.PlanningEffect_READ, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, test.action)
			request.CommonSteps = []*protocol.CommonPlanningStep{commonPath(request, test.common)}
			result, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			if test.refused {
				c.Assert(result.Diagnostics, qt.HasLen, 1)
				c.Assert(result.Contributions, qt.HasLen, 0)
				c.Assert(result.Changes, qt.HasLen, 0)
				return
			}
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			edges := result.Contributions[0].Dependencies
			c.Assert(edges, qt.HasLen, 1)
			c.Assert(edges[0].Before.Owner, qt.Equals, test.before)
		})
	}
}

func TestCoordinationPlanningRefusesWholeBatch(t *testing.T) {
	c := qt.New(t)
	request := nativePlan(c, "create")
	second := proto.CloneOf(request.Changes[0])
	second.Subject.Name.Source = "other"
	second.Subject.Name.Normalized = "other"
	second.Value.Id = "other"
	second.Value.Value.Owner.Name = "other"
	second.Value.NormalizedOwner.Name = "other"
	request.Changes = append(request.Changes, second)
	conflict := commonPath(request, protocol.PlanningEffect_CREATE)
	conflict.Metadata.Effects[0].Subject.Name = proto.CloneOf(second.Subject.Name)
	request.CommonSteps = []*protocol.CommonPlanningStep{conflict}
	result, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
	request.CommonSteps = nil
	delete(request.Capabilities, "transactional_ddl")
	result, err = backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	request.Capabilities["transactional_ddl"] = false
	request.Changes[0].Value.ChangeSemantics.RemovesState = new(true)
	result, err = backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = backend.New().PlanFeatures(ctx, request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationOperationRefusesSemanticNoop(t *testing.T) {
	c := qt.New(t)
	request := nativePlan(c, "alter")
	input := request.Changes[0]
	before, _, err := schema.CoordinationOperands(input.Value.Value)
	c.Assert(err, qt.IsNil)
	desired, err := schema.CaptureCoordinationIntent(input.Value.Value.Owner, schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	input.Value, err = schema.CaptureCoordinationChange(input.Value.Id, input.Value.Value.Owner, before, desired)
	c.Assert(err, qt.IsNil)
	result, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Contributions, qt.HasLen, 0)
}

func TestCoordinationPlanningRefusesUnestablishedFootprints(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*protocol.CommonPlanningStep)
	}{
		{"unbound database", func(s *protocol.CommonPlanningStep) { s.Metadata.Effects[0].Subject.Database = nil }},
		{"unknown kind", func(s *protocol.CommonPlanningStep) { s.Metadata.Effects[0].Subject.Kind = "foreign-object" }},
		{"directory", func(s *protocol.CommonPlanningStep) { s.Metadata.Effects[0].Subject.Kind = "schema" }},
		{"folded name", func(s *protocol.CommonPlanningStep) { s.Metadata.Effects[0].Subject.Name.Source = "NODE" }},
		{"ancestor", func(s *protocol.CommonPlanningStep) {
			s.Metadata.Effects[0].Subject.Schema = nil
			s.Metadata.Effects[0].Subject.Name.Source = "app"
			s.Metadata.Effects[0].Subject.Name.Normalized = "app"
		}},
		{"descendant", func(s *protocol.CommonPlanningStep) {
			s.Metadata.Effects[0].Subject.Schema.Source = "app/node"
			s.Metadata.Effects[0].Subject.Schema.Normalized = "app/node"
		}},
		{"no effects", func(s *protocol.CommonPlanningStep) { s.Metadata.Effects = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, "create")
			common := commonPath(request, protocol.PlanningEffect_DROP)
			test.modify(common)
			request.CommonSteps = []*protocol.CommonPlanningStep{common}
			result, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
		})
	}
}

func TestCoordinationRenderRefusesWholeBatch(t *testing.T) {
	c := qt.New(t)
	request := nativePlan(c, "create")
	plan, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	second := proto.CloneOf(rendering.Changes[0])
	second.Id = "second"
	second.GetNative().Value.Id = "second"
	second.GetNative().Value.Value.Payload = []byte(`{"before":null,"after":{"unknown":true}}`)
	rendering.Changes = append(rendering.Changes, second)
	result, err := backend.New().Render(t.Context(), rendering)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
	rendering.Changes = rendering.Changes[:1]
	delete(rendering.Capabilities, "transactional_ddl")
	result, err = backend.New().Render(t.Context(), rendering)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationPlanningKeepsExplicitDatabasesSeparate(t *testing.T) {
	c := qt.New(t)
	request := nativePlan(c, "create")
	common := commonPath(request, protocol.PlanningEffect_CREATE)
	common.Metadata.Effects[0].Subject.Database.Source = "/other"
	common.Metadata.Effects[0].Subject.Database.Normalized = "/other"
	request.CommonSteps = []*protocol.CommonPlanningStep{common}
	result, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Contributions[0].Dependencies, qt.HasLen, 0)
}
