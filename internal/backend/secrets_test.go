package backend_test

import (
	"context"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
			c := testcheck.New(t)
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

// secretRead is what the read of the database holds of the secret.
type secretRead int

const (
	// secretAbsent is a read that holds no secret at the name.
	secretAbsent secretRead = iota
	// secretPresent is a read that holds the secret.
	secretPresent
)

func secretRequest(c *qt.C, read secretRead) *protocol.CompareModelsRequest {
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
	if read == secretPresent {
		readOwner := proto.CloneOf(owner)
		readOwner.Database = "/local"
		value, err := schema.CaptureObject(readOwner, &native.SecretPresence{})
		c.Assert(err, qt.IsNil)
		add(current, value)
	}
	return &protocol.CompareModelsRequest{Engine: "ydb", DatabasePath: "/local", ComparisonId: secretcompare.ServiceID, Kinds: []string{schema.SecretProperties}, TargetFeatures: backend.Description().Features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"secrets": true, "transactional_ddl": false}, Desired: desired, Current: current}
}

func TestSecretsPresenceAndExplicitRotation(t *testing.T) {
	for _, test := range []struct {
		name    string
		read    secretRead
		rotate  bool
		changes int
		impact  protocol.ChangeSemantics_Impact
	}{
		{name: "create", changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "presence ignores value reference", read: secretPresent},
		{name: "explicit rotation", read: secretPresent, rotate: true, changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "rotation of absent secret creates", rotate: true, changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := secretRequest(c, test.read)
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
				rendered, err := backend.New().Render(c.Context(), testplanning.RenderRequest(backend.Description(), plan, request.Capabilities))
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
			c := testcheck.New(t)
			request := secretRequest(c, secretPresent)
			request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: written}}
			result, err := backend.New().CompareModels(c.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
	c := testcheck.New(t)
	request := secretRequest(c, secretAbsent)
	request.Current.Coverage.Kinds[0].Sources[0].Regions = nil
	request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: "app/pg.pw"}}
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
}

func TestSecretComparisonKeepsUnsupportedObservationsUndecided(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretPresent)
	request.Capabilities["secrets"] = false
	request.Desired.Records = nil
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
}

func TestSecretSourceRefusesInlineValuesAndPreservesReferences(t *testing.T) {
	c := testcheck.New(t)
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
	c := testcheck.New(t)
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

// A source that leaves out a secret a declared object reads by its path asks
// for a drop that would leave the reader naming a secret that is gone, since
// YDB drops a secret a data source still names. The comparison refuses it by
// name, as the host's engine refuses a plan that drops a secret one of its
// statements reads.
func TestSecretComparisonRefusesDroppingASecretADeclarationReads_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretPresent)
	request.Required = []*protocol.ComparisonIdentity{proto.CloneOf(request.Desired.Records[0].Subject)}
	request.Desired.Records = nil

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = secret app/pg.pw is dropped while a declared object reads it by its path`)
	c.Assert(result, qt.IsNil)
}

// The control: the same omission with nothing declared reading the secret is
// a drop.
func TestSecretComparisonDropsASecretNoDeclarationReads_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretPresent)
	request.Desired.Records = nil

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Undecided, qt.HasLen, 0)
}

// A user created with a password input is a unit whose discovery reports the
// input and whose statement names it, so the host approves the reference and
// binds the value at execution (ADR 0071).
func TestPasswordInputDiscoveryReportsTheUsersInput_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	units := []*protocol.Unit{
		{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: "CREATE USER `app` PASSWORD $PTAH_SECRET_PASSWORD_APP;"}},
		{ChangeId: "alter", Body: &protocol.Unit_Sql{Sql: "ALTER USER `ops` PASSWORD $PTAH_SECRET_PASSWORD_OPS NOLOGIN;"}},
	}

	inputs, err := backend.New().DescribeExecutionInputs(c.Context(), &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: units})

	c.Assert(err, qt.IsNil)
	c.Assert(inputs.GetInputs(), qt.HasLen, 2)
	c.Assert([]string{inputs.Inputs[0].Name, inputs.Inputs[1].Name}, qt.DeepEquals, []string{"PTAH_SECRET_PASSWORD_APP", "PTAH_SECRET_PASSWORD_OPS"})
}

// A reference to a password input anywhere but after PASSWORD is refused
// before anything runs, as a secret's is.
func TestPasswordInputDiscoveryRefusesAnotherPosition_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	unit := &protocol.Unit{ChangeId: "read", Body: &protocol.Unit_Sql{Sql: "CREATE USER `app` PASSWORD $PTAH_SECRET_PASSWORD_APP; SELECT $PTAH_SECRET_PASSWORD_APP;"}}

	inputs, err := backend.New().DescribeExecutionInputs(c.Context(), &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: []*protocol.Unit{unit}})

	c.Assert(err, qt.ErrorMatches, `.*a secret value is referred to outside the value of CREATE SECRET or ALTER SECRET and the password of CREATE USER or ALTER USER.*`)
	c.Assert(inputs, qt.IsNil)
}

// plannedSecret is the captured change of the secret at directory/name of
// the fixture database: action "create" declares it with the variable
// PTAH_SECRET_PW, "drop" reads it and declares nothing, and "rotate" does
// both.
func plannedSecret(c *qt.C, directory, name, action string) *protocol.PlanningChange {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: directory, Name: name}
	var before, after *protocol.Envelope
	var err error
	if action != "create" {
		before, err = schema.CaptureObject(owner, &native.SecretPresence{})
		c.Assert(err, qt.IsNil)
	}
	if action != "drop" {
		after, err = schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PW"})
		c.Assert(err, qt.IsNil)
	}
	return plannedSecretChange(c, owner, before, after)
}

// plannedSecretChange is the captured change of the secret owner names from
// before to after, either of which may be nil.
func plannedSecretChange(c *qt.C, owner *protocol.Reference, before, after *protocol.Envelope) *protocol.PlanningChange {
	c.Helper()
	change, err := schema.CaptureSecretChange(owner.Schema+"/"+owner.Name, owner, before, after)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: testplanning.Subject(owner), Value: change}
}

// planSecrets plans changes with the secret service on a target with
// secrets, beside the host's steps common.
func planSecrets(c *qt.C, common []*protocol.CommonPlanningStep, changes ...*protocol.PlanningChange) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: secretplan.ServiceID, DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"secrets": true, "transactional_ddl": false},
		Changes: changes, CommonSteps: common}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return request, result
}

// Each created, rotated or dropped secret is one statement that names the
// declared variable, or, for a secret read from a database and declared
// again, such as one a rollback restores, the variable Ptah names after the
// secret's path. Each step acts on the secret and on its path, and says what
// the statement does to the value. Ported from the host's
// TestSecretPlan_LowersEachChange and TestSecretEffect.
func TestSecretPlanLowersEachChange_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	restoredOwner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Name: "restored.pw"}
	read, err := schema.CaptureObject(restoredOwner, &native.SecretPresence{})
	c.Assert(err, qt.IsNil)
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "restored", Representation: protocol.ValueInput_OBSERVED, Value: read}}})
	c.Assert(err, qt.IsNil)
	converted, err := backend.New().ConvertValues(c.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: normalized.Values})
	c.Assert(err, qt.IsNil)
	createdOwner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "ext", Name: "created"}
	declared, err := schema.CaptureSecretIntent(createdOwner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_CREATED"})
	c.Assert(err, qt.IsNil)
	created := plannedSecretChange(c, createdOwner, nil, declared)
	rotatedOwner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "ext", Name: "rotated"}
	present, err := schema.CaptureObject(rotatedOwner, &native.SecretPresence{})
	c.Assert(err, qt.IsNil)
	declared, err = schema.CaptureSecretIntent(rotatedOwner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_ROTATED"})
	c.Assert(err, qt.IsNil)
	rotated := plannedSecretChange(c, rotatedOwner, present, declared)
	request, plan := planSecrets(c, nil, created, plannedSecretChange(c, restoredOwner, nil, converted.Values[0].Value), rotated, plannedSecret(c, "ext", "dropped", "drop"))
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT

	rendered, err := backend.New().Render(c.Context(), rendering)

	c.Assert(err, qt.IsNil)
	type lowered struct {
		Statement, Effects string
		Impact             protocol.ChangeSemantics_Impact
		Reason             string
	}
	var got []lowered
	for i, step := range plan.Contributions[0].Steps {
		var effects []string
		for _, effect := range step.Metadata.Effects {
			effects = append(effects, effect.Action.String()+" "+effect.Subject.Kind+" "+effect.Subject.Schema.GetNormalized()+"|"+effect.Subject.Name.GetNormalized())
		}
		got = append(got, lowered{Statement: rendered.Units[i].GetSql(), Effects: strings.Join(effects, ", "), Impact: step.Metadata.Impact, Reason: step.Metadata.Reason})
	}
	c.Assert(got, qt.DeepEquals, []lowered{
		{Statement: "CREATE SECRET `restored.pw` WITH (value = $PTAH_SECRET_RESTORED_PW);",
			Effects: "CREATE " + schema.SecretProperties + " |restored.pw, CREATE ydb.scheme-path |restored.pw",
			Impact:  protocol.ChangeSemantics_ADDITIVE, Reason: secrets.CreateReason},
		{Statement: "CREATE SECRET `ext/created` WITH (value = $PTAH_SECRET_CREATED);",
			Effects: "CREATE " + schema.SecretProperties + " ext|created, CREATE ydb.scheme-path ext|created, READ ydb.scheme-directory |ext",
			Impact:  protocol.ChangeSemantics_ADDITIVE, Reason: secrets.CreateReason},
		{Statement: "DROP SECRET `ext/dropped`;",
			Effects: "DROP " + schema.SecretProperties + " ext|dropped, DROP ydb.scheme-path ext|dropped, CREATE ydb.scheme-directory ext|dropped",
			Impact:  protocol.ChangeSemantics_DESTRUCTIVE, Reason: secrets.DropReason},
		{Statement: "ALTER SECRET `ext/rotated` WITH (value = $PTAH_SECRET_ROTATED);",
			Effects: "ALTER " + schema.SecretProperties + " ext|rotated, ALTER ydb.scheme-path ext|rotated",
			Impact:  protocol.ChangeSemantics_BEHAVIORAL, Reason: secrets.RotateReason},
	})
}

// A secret planned beside the host's steps at its path or a path above it
// takes the path once it is free: a creation follows the drop of the object
// at its path or at the directory above it, and a drop precedes the creation
// of an object at its path. A creation and a rotation act on the secret
// itself, which is what orders an external data source or a replication that
// reads the secret by its path after them. Ported from the host's
// TestSecretPlan_PlacesEachStatement.
func TestSecretOrdersAgainstCommonStepsAtItsPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action string
		common       []*protocol.CommonPlanningStep
		edges        []string
		action0      protocol.PlanningEffect_Action
	}{
		{name: "a creation follows the drop that frees its path", action: "create",
			common: []*protocol.CommonPlanningStep{commonAt("table", "ext", "pw", protocol.PlanningEffect_DROP)},
			edges:  []string{"drop table ext/pw -> secret/000000"}, action0: protocol.PlanningEffect_CREATE},
		{name: "a creation follows the drop of its directory", action: "create",
			common: []*protocol.CommonPlanningStep{commonAt("table", "", "ext", protocol.PlanningEffect_DROP)},
			edges:  []string{"drop table /ext -> secret/000000"}, action0: protocol.PlanningEffect_CREATE},
		{name: "a drop runs before a creation at its path", action: "drop",
			common: []*protocol.CommonPlanningStep{commonAt("table", "ext", "pw", protocol.PlanningEffect_CREATE)},
			edges:  []string{"secret/000000 -> create table ext/pw"}, action0: protocol.PlanningEffect_DROP},
		{name: "a creation with nothing at its path", action: "create", action0: protocol.PlanningEffect_CREATE},
		{name: "a rotation", action: "rotate", action0: protocol.PlanningEffect_ALTER},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			request, result := planSecrets(c, test.common, plannedSecret(c, "ext", "pw", test.action))

			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(stepEdges(result), qt.DeepEquals, test.edges)
			effects := result.Contributions[0].Steps[0].Metadata.Effects
			c.Assert(effects[0].Subject.Kind, qt.Equals, schema.SecretProperties)
			c.Assert(effects[0].Action, qt.Equals, test.action0)
		})
	}
}

// A secret dropped below paths where the plan creates objects is refused
// before anything runs. YDB keeps the directory above the secret after the
// drop, and YQL has no statement that removes one, so the creation above
// fails whatever the order. The host's planner refused the same plan as a
// whole, in these words, after its secret service had ordered the drop first.
// Ported from the host's TestSecretPlan_DropPrecedesACreationAboveIt.
func TestSecretDropBelowAPathThePlanCreates_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	common := []*protocol.CommonPlanningStep{commonAt("table", "", "app", protocol.PlanningEffect_CREATE), commonAt("table", "app", "ext", protocol.PlanningEffect_CREATE)}

	_, result := planSecrets(c, common, plannedSecret(c, "app/ext", "pw", "drop"))

	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, "this plan drops app/ext/pw and creates an object at app, which is a directory in the database. .*")
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
}

// One refused secret change refuses the batch: a target without secrets, and
// a creation at a path a host step creates a table at. The diagnostic names
// the refused change, and no other change of the batch is planned. Ported
// from the host's TestSecretPlan_RefusesTheWholeBatch.
func TestSecretPlanRefusesTheWholeBatch_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		secrets bool
		common  []*protocol.CommonPlanningStep
		change  uint32
		want    string
	}{
		{name: "a target without secrets", change: 0, want: "Planning a secret requires the secrets capability and nontransactional DDL.*"},
		{name: "a path a table takes", secrets: true, change: 1,
			common: []*protocol.CommonPlanningStep{commonAt("table", "ext", "pw", protocol.PlanningEffect_CREATE)},
			want:   "secret operation conflicts with a common occupant at its path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: secretplan.ServiceID, DatabasePath: "/local",
				Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
				Capabilities: map[string]bool{"secrets": test.secrets, "transactional_ddl": false}, CommonSteps: test.common,
				Changes: []*protocol.PlanningChange{plannedSecret(c, "", "first", "create"), plannedSecret(c, "ext", "pw", "create")}}

			result, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].GetChange(), qt.Equals, test.change)
			c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, test.want)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 0)
		})
	}
}

// A source that leaves out a secret the read holds plans the secret's drop,
// which destroys its value: nothing reads a secret's value back. Ported from
// the host's TestSecretChange_Effect, TestClassify_Secret and
// TestClassifySchemaDiff_Secrets.
func TestSecretComparisonDropIsDestructive_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretPresent)
	request.Desired.Records = nil

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	semantics := result.Changes[0].Value.ChangeSemantics
	c.Assert(semantics.Impact, qt.Equals, protocol.ChangeSemantics_DESTRUCTIVE)
	c.Assert(semantics.Reason, qt.Equals, secrets.DropReason)
	c.Assert(semantics.GetRemovesState(), qt.IsTrue)
}

// rotationRequest is secretRequest for the secret declared at directory/name
// and read present there, with one rotation request written as written.
func rotationRequest(c *qt.C, directory, name, written string) *protocol.CompareModelsRequest {
	c.Helper()
	request := secretRequest(c, secretPresent)
	request.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.Schema = testcomparison.Part(directory)
	for _, state := range []*protocol.ComparisonState{request.Desired, request.Current} {
		record := state.Records[0]
		owner := proto.CloneOf(record.Value.Value.Owner)
		owner.Schema, owner.Name = directory, name
		var value *protocol.Envelope
		var err error
		if state == request.Desired {
			value, err = schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
		} else {
			value, err = schema.CaptureObject(owner, &native.SecretPresence{})
		}
		c.Assert(err, qt.IsNil)
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "secret", Representation: state.Representation, Value: value}}})
		c.Assert(err, qt.IsNil)
		record.Value = normalized.Values[0]
		record.Subject.Schema, record.Subject.Name = testcomparison.Part(directory), testcomparison.Part(name)
	}
	request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: written}}
	return request
}

// A rotation request names the secret by the path YDB writes for it: a
// dotted name at the root, and dots inside directory names. The protocol
// carries the path as one trimmed line, so space around it never reaches the
// provider. Ported from the host's TestParsePath_HappyPath.
func TestSecretRotationReadsTheWrittenPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, directory, leaf, written string
	}{
		{name: "a dotted root name", leaf: "pg.pw", written: "pg.pw"},
		{name: "a directory", directory: "ext", leaf: "pg", written: "ext/pg"},
		{name: "dotted segments", directory: "a/b.c", leaf: "d.e", written: "a/b.c/d.e"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := rotationRequest(c, test.directory, test.leaf, test.written)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(result.ResolvedRequests, qt.HasLen, 1)
			c.Assert(result.ResolvedRequests[0].Subject.Schema.Source, qt.Equals, test.directory)
			c.Assert(result.ResolvedRequests[0].Subject.Name.Source, qt.Equals, test.leaf)
		})
	}
}

// A rotation request written as the parent of the root, with a current or a
// parent segment for its directory, or with no name after its directory names
// no secret path, and is refused in those words before the comparison looks
// for a declared secret. An empty path never reaches the provider: the
// protocol refuses a written request without text. Ported from the host's
// TestParsePath_FailurePath.
func TestSecretRotationRefusesAWrittenPathThatIsNoSecretPath_FailurePath(t *testing.T) {
	for _, written := range []string{"..", "./pw", "ext/", "ext/../pw"} {
		t.Run(written, func(t *testing.T) {
			c := testcheck.New(t)
			request := rotationRequest(c, "ext", "pw", written)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.ErrorMatches, `.*secret requires a relative directory and an exact leaf name`)
			c.Assert(result, qt.IsNil)
		})
	}
}
