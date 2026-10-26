package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func TestExternalSecretDependencies(t *testing.T) {
	source := func(reference string) external {
		return external{name: "pg", source: &ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "BASIC",
			Options: map[string]string{"LOGIN": "reader", "PASSWORD_SECRET_PATH": reference}}}
	}
	for _, test := range []struct {
		name             string
		desired, current []external
		reads            []string
		placement        protocol.PlanningMetadata_Placement
	}{
		{name: "create after secret", desired: []external{source("/local/app/new.pw")}, reads: []string{"new.pw"}},
		{name: "drop before secret", current: []external{source("app/old.pw")}, reads: []string{"old.pw"}, placement: protocol.PlanningMetadata_EARLY},
		{name: "replace before old secret removal", desired: []external{source("app/new.pw")}, current: []external{source("app/old.pw")}, reads: []string{"new.pw", "old.pw"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, plan := externalPlan(c, test.desired, test.current, withSources(), withSources())
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
			c.Assert(step.Metadata.Placement, qt.Equals, test.placement)
			var reads, slots []string
			for _, effect := range step.Metadata.Effects {
				if effect.Action != protocol.PlanningEffect_READ {
					continue
				}
				c.Assert(effect.Subject.Database.GetNormalized(), qt.Equals, "")
				c.Assert(effect.Subject.Schema.GetNormalized(), qt.Equals, "app")
				switch effect.Subject.Kind {
				case schema.SecretProperties:
					reads = append(reads, effect.Subject.Name.Normalized)
				case objectplan.PathKind:
					slots = append(slots, effect.Subject.Name.Normalized)
				}
			}
			c.Assert(reads, qt.DeepEquals, test.reads)
			c.Assert(slots, qt.DeepEquals, test.reads)
		})
	}
}

func secretRequest(c *qt.C, present bool) *protocol.CompareModelsRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: "app", Name: "pg.pw"}
	var observedModel *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == schema.SecretProperties {
			observedModel = model
		}
	}
	desired, current := testcomparison.State(schema.NativeDesiredModel(schema.SecretProperties)), testcomparison.State(observedModel)
	current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	current.Coverage.Kinds[0].Sources[0].Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Targets: []string{"ydb"}, SubjectKinds: []string{schema.SecretProperties}, Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""), Schema: testcomparison.Part("app")}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
	add := func(state *protocol.ComparisonState, value *protocol.Envelope) {
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "secret", Representation: state.Representation, Value: value}}})
		c.Assert(err, qt.IsNil)
		state.Records = []*protocol.ComparisonRecord{{Kind: schema.SecretProperties, Subject: &protocol.ComparisonIdentity{Kind: schema.SecretProperties, Database: testcomparison.Part(value.Owner.Database), Catalog: testcomparison.Part(""), Schema: testcomparison.Part("app"), Parent: testcomparison.Part(""), Name: testcomparison.Part("pg.pw")}, Value: normalized.Values[0]}}
	}
	value, err := schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
	c.Assert(err, qt.IsNil)
	add(desired, value)
	if present {
		readOwner := proto.CloneOf(owner)
		readOwner.Database = "/local"
		value, err := schema.CaptureObject(readOwner, &native.SecretPresence{})
		c.Assert(err, qt.IsNil)
		add(current, value)
	}
	return &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: secretcompare.ServiceID, Kinds: []string{schema.SecretProperties}, TargetFeatures: backend.Description().Features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"secrets": true, "transactional_ddl": false}, Desired: desired, Current: current}
}

func TestSecretsPresenceAndExplicitRotation(t *testing.T) {
	for _, test := range []struct {
		name            string
		present, rotate bool
		changes         int
		impact          protocol.ChangeSemantics_Impact
	}{
		{name: "create", changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "presence ignores value reference", present: true},
		{name: "explicit rotation", present: true, rotate: true, changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "rotation of absent secret creates", rotate: true, changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := secretRequest(c, test.present)
			if test.rotate {
				request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: "app/pg.pw"}}
			}
			before := proto.CloneOf(request)
			result, err := backend.New().CompareModels(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(proto.Equal(before, request), qt.IsTrue)
			if test.rotate {
				c.Assert(result.ResolvedRequests, qt.HasLen, 1)
				c.Assert(result.ResolvedRequests[0].Subject.Name.Source, qt.Equals, "pg.pw")
			}
			for _, change := range result.Changes {
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, test.impact)
				planRequest := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: secretplan.ServiceID, TargetFeatures: request.TargetFeatures, Identifiers: request.Identifiers, Capabilities: request.Capabilities, Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}}
				plan, err := backend.New().PlanFeatures(c.Context(), planRequest)
				c.Assert(err, qt.IsNil)
				c.Assert(plan.Diagnostics, qt.HasLen, 0)
				rendered, err := backend.New().Render(c.Context(), testplanning.RenderRequest(plan, request.Capabilities))
				c.Assert(err, qt.IsNil)
				inputs, err := backend.New().DescribeExecutionInputs(c.Context(), &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: rendered.Units})
				c.Assert(err, qt.IsNil)
				c.Assert(inputs.Inputs, qt.HasLen, 1)
				c.Assert(inputs.Inputs[0].Name, qt.Equals, "PTAH_SECRET_PG")
			}
		})
	}
}

func TestSecretRotationRefusesUndeclaredAndUnknownPresence(t *testing.T) {
	for _, written := range []string{"missing", "app/missing", "/local/app/pg.pw", "../app/pg.pw", "app//pg.pw"} {
		t.Run(written, func(t *testing.T) {
			c := qt.New(t)
			request := secretRequest(c, true)
			request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: written}}
			result, err := backend.New().CompareModels(c.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
	c := qt.New(t)
	request := secretRequest(c, false)
	request.Current.Coverage.Kinds[0].Sources[0].Regions = nil
	request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: "app/pg.pw"}}
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
}

func TestSecretComparisonKeepsUnsupportedObservationsUndecided(t *testing.T) {
	c := qt.New(t)
	request := secretRequest(c, true)
	request.Capabilities["secrets"] = false
	request.Desired.Records = nil
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
}

func TestSecretSourceRefusesInlineValuesAndPreservesReferences(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.YAMLDocument(`{"secrets":{"a.b":{"schema":"app","value_env":"PTAH_SECRET_PG"}}}`, secretsource.YAMLKey(), schema.NativeDesiredModel(schema.SecretProperties))
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := result.Documents[0].Fragments[0].Values[0].Value.Value
	c.Assert(value.Owner.Name, qt.Equals, "a.b")
	c.Assert(value.Owner.Schema, qt.Equals, "app")
	for _, bad := range []string{`{"value":"DO_NOT_REPORT"}`, `{"value_env":"DO_NOT_REPORT"}`, `{"value_env":"PTAH_SECRET_X","unknown":"DO_NOT_REPORT"}`, `{"value_env":null}`, `{"value_env":"PTAH_SECRET_X","name":"../escape"}`} {
		request, err := testsource.YAMLDocument(`{"secrets":{"bad":`+bad+`}}`, secretsource.YAMLKey(), schema.NativeDesiredModel(schema.SecretProperties))
		c.Assert(err, qt.IsNil)
		result, err := backend.New().DecodeSource(c.Context(), request)
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
		c.Assert(err.Error(), qt.Not(qt.Contains), "DO_NOT_REPORT")
	}
}

func TestSecretInputDiscoveryRejectsAnUnsafeSuffixBeforeExecution(t *testing.T) {
	c := qt.New(t)
	p := backend.New()
	valid := &protocol.Unit{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: "CREATE SECRET s WITH (value = $PTAH_SECRET_S);"}}
	inputs, err := p.DescribeExecutionInputs(c.Context(), &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: []*protocol.Unit{valid}})
	c.Assert(err, qt.IsNil)
	c.Assert(inputs.Inputs[0].Name, qt.Equals, "PTAH_SECRET_S")
	for _, suffix := range []string{"SELECT $PTAH_SECRET_S;", "$PTAH_SECRET_S = 'literal'; SELECT 1;", "CREATE SECRET s WITH (value = $PTAH_SECRET_S || 'suffix');"} {
		request := &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: []*protocol.Unit{valid, {ChangeId: "unsafe", Body: &protocol.Unit_Sql{Sql: suffix}}}}
		result, err := p.DescribeExecutionInputs(c.Context(), request)
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
	}
	ctx := provider.ContextWithHostFeatures(context.Background(), []string{provider.SensitiveInputsFeature})
	execution, err := p.Execute(ctx, &protocol.ExecuteRequest{Engine: "ydb", SessionId: "no-session", RequestId: "test", Units: []*protocol.Unit{valid}, ExpectedInputs: inputs.Inputs})
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(execution.DiagnosticCode, qt.Equals, "ydb.invalid-execution-inputs")
}
