package backend_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// workloadCapabilities are a target whose EnableResourcePools flag is on.
func workloadCapabilities() map[string]bool {
	return map[string]bool{workload.CapabilityResourcePools: true, "transactional_ddl": false}
}

// workloadSubject is a pool or a classifier named as the host names it on
// the side given: a declaration without a database, a read with the
// session's.
func workloadSubject(kind, name, database string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part(database), Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{},
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
}

// workloadRequest captures a comparison of the objects of one kind, each
// declared or read as given; the read side covers the whole database, as an
// inspection on a target with resource pools does.
func workloadRequest(c *qt.C, kind string, declared, read map[string]proto.Message) *protocol.CompareModelsRequest {
	c.Helper()
	var observedModel *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == kind {
			observedModel = model
		}
	}
	service := workloadcompare.PoolServiceID
	if kind == schema.ResourcePoolClassifierProperties {
		service = workloadcompare.ClassifierServiceID
	}
	desiredState, currentState := testcomparison.State(schema.NativeDesiredModel(kind)), testcomparison.State(observedModel)
	request := &protocol.CompareModelsRequest{Engine: "ydb", DatabasePath: "/local", ComparisonId: service, Kinds: []string{kind},
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: workloadCapabilities(),
		Desired: desiredState, Current: currentState}
	for _, side := range []struct {
		state    *protocol.ComparisonState
		values   map[string]proto.Message
		database string
	}{{desiredState, declared, ""}, {currentState, read, "/local"}} {
		for name, settings := range side.values {
			owner := &protocol.Reference{Engine: "ydb", Kind: kind, Database: side.database, Name: name}
			var value *protocol.Envelope
			var err error
			if side.state == desiredState {
				value, err = schema.CaptureWorkloadIntent(owner, settings)
			} else {
				value, err = schema.CaptureWorkloadObservation(owner, settings)
			}
			c.Assert(err, qt.IsNil)
			normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: name, Representation: side.state.Representation, Value: value}}})
			c.Assert(err, qt.IsNil)
			side.state.Records = append(side.state.Records, &protocol.ComparisonRecord{Kind: kind, Subject: workloadSubject(kind, name, side.database), Value: normalized.Values[0]})
		}
	}
	return request
}

func pool(spec workload.PoolSpec) proto.Message { return schema.PoolSettings(spec) }

func routing(pool string, rank int64) proto.Message {
	return schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: pool, Rank: rank})
}

func compareWorkload(c *qt.C, request *protocol.CompareModelsRequest) *protocol.ComparedModels {
	c.Helper()
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	return result
}

// A pool compares setting by setting and a classifier as a whole, as the host
// compares them; each change says what it costs.
func TestWorkloadComparison_HappyPath(t *testing.T) {
	limited := workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}
	for _, test := range []struct {
		name           string
		kind           string
		declared, read map[string]proto.Message
		changes        int
		impact         protocol.ChangeSemantics_Impact
	}{
		{name: "the same pool", kind: schema.ResourcePoolProperties, declared: map[string]proto.Message{"batch": pool(limited)}, read: map[string]proto.Message{"batch": pool(limited)}},
		{name: "a new pool", kind: schema.ResourcePoolProperties, declared: map[string]proto.Message{"batch": pool(limited)}, changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "a zero limit is a limit", kind: schema.ResourcePoolProperties, declared: map[string]proto.Message{"batch": pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0))})},
			read: map[string]proto.Message{"batch": pool(workload.PoolSpec{})}, changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "the default pool's weight", kind: schema.ResourcePoolProperties, declared: map[string]proto.Message{"default": pool(workload.PoolSpec{ResourceWeight: new(30.0)})},
			read: map[string]proto.Message{"default": pool(workload.PoolSpec{})}, changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "the same classifier", kind: schema.ResourcePoolClassifierProperties, declared: map[string]proto.Message{"etl": routing("batch", 10)}, read: map[string]proto.Message{"etl": routing("batch", 10)}},
		{name: "a classifier moved", kind: schema.ResourcePoolClassifierProperties, declared: map[string]proto.Message{"etl": routing("batch", 20)}, read: map[string]proto.Message{"etl": routing("batch", 10)},
			changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compareWorkload(c, workloadRequest(c, test.kind, test.declared, test.read))
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			for _, change := range result.Changes {
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			}
		})
	}
}

// A database-wide object the source leaves out is kept as YDB holds it, even
// where the source describes the kind completely: a source declares the
// workload objects it manages, not every one of the database.
func TestWorkloadComparisonKeepsAnOmittedObject_HappyPath(t *testing.T) {
	for _, kind := range []string{schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties} {
		t.Run(kind, func(t *testing.T) {
			c := qt.New(t)
			read := map[string]proto.Message{"default": pool(workload.PoolSpec{})}
			if kind == schema.ResourcePoolClassifierProperties {
				read = map[string]proto.Message{"etl": routing("batch", 10)}
			}
			result := compareWorkload(c, workloadRequest(c, kind, nil, read))
			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Desired.Records, qt.HasLen, 1)
			c.Assert(result.Desired.Records[0].Value.Value.Kind, qt.Equals, kind)
		})
	}
}

// What the comparison cannot decide it says, with the host's words.
func TestWorkloadComparison_Undecided(t *testing.T) {
	c := qt.New(t)
	absent := workloadRequest(c, schema.ResourcePoolProperties, nil, map[string]proto.Message{"batch": pool(workload.PoolSpec{})})
	absent.Desired.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.ResourcePoolProperties, Subject: workloadSubject(schema.ResourcePoolProperties, "batch", ""),
		Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_ABSENT)}}
	unreadDefault := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"default": pool(workload.PoolSpec{ResourceWeight: new(1.0)})}, nil)
	uninspected := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"batch": pool(workload.PoolSpec{})}, nil)
	uninspected.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	for _, test := range []struct {
		name    string
		request *protocol.CompareModelsRequest
		want    string
	}{
		{"an absence the source claims", absent, "removing a database-wide workload object requires an explicit operation, not source omission"},
		{"the default pool unread", unreadDefault, "the database's default pool must be inspected before changing its settings"},
		{"a target that was not read", uninspected, "The current resource pool or its absence was not established"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compareWorkload(c, test.request)
			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, 1)
			c.Assert(result.Undecided[0].Reason, qt.Equals, test.want)
		})
	}
}

// A change a target without resource pools would need is refused; a
// comparison that changes nothing needs no capability.
func TestWorkloadComparison_FailurePath(t *testing.T) {
	c := qt.New(t)
	request := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"batch": pool(workload.PoolSpec{})}, nil)
	request.Capabilities = map[string]bool{}
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*changing resource pools requires resource_pools`)
	c.Assert(result, qt.IsNil)

	kept := workloadRequest(c, schema.ResourcePoolProperties, nil, map[string]proto.Message{"default": pool(workload.PoolSpec{})})
	kept.Capabilities = map[string]bool{}
	c.Assert(compareWorkload(c, kept).Changes, qt.HasLen, 0)
}

// workloadPlan compares each request and plans every change in one request,
// as the host sends the changes of the planner's kinds together.
func workloadPlan(c *qt.C, capabilities map[string]bool, common []*protocol.CommonPlanningStep, requests ...*protocol.CompareModelsRequest) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: workloadplan.ServiceID, Identifiers: requests[0].Identifiers, Capabilities: capabilities, CommonSteps: common}
	for _, request := range requests {
		for _, change := range compareWorkload(c, request).Changes {
			// Each comparison numbers its own changes; a planning request
			// holds every change under an ID of its own.
			change.Value.Id = request.ComparisonId + "/" + change.Value.Id
			planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
		}
	}
	plan, err := backend.New().PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	return planning, plan
}

// Every workload change of a plan is one native step that forbids a
// transaction, with an effect and a receipt per object, that renders as a
// native unit and as the host's statements.
func TestWorkloadPlanning_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name                         string
		pools                        map[string]proto.Message
		classifiers, readClassifiers map[string]proto.Message
		actions                      []protocol.PlanningEffect_Action
		impact                       protocol.ChangeSemantics_Impact
		text                         string
	}{
		{
			name:        "a pool and a classifier routing to it",
			pools:       map[string]proto.Message{"batch": pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))})},
			classifiers: map[string]proto.Message{"etl": routing("batch", 10)},
			actions:     []protocol.PlanningEffect_Action{protocol.PlanningEffect_CREATE, protocol.PlanningEffect_CREATE},
			impact:      protocol.ChangeSemantics_BEHAVIORAL,
			text: "CREATE RESOURCE POOL `batch` WITH (CONCURRENT_QUERY_LIMIT = 2);\n" +
				"CREATE RESOURCE POOL CLASSIFIER `etl` WITH (RESOURCE_POOL = 'batch', RANK = 10);",
		},
		{
			name:            "two classifiers trading ranks",
			classifiers:     map[string]proto.Message{"alpha": routing("batch", 20), "beta": routing("batch", 10)},
			readClassifiers: map[string]proto.Message{"alpha": routing("batch", 10), "beta": routing("batch", 20)},
			actions:         []protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER},
			impact:          protocol.ChangeSemantics_BEHAVIORAL,
			text: "DROP RESOURCE POOL CLASSIFIER `alpha`;\nDROP RESOURCE POOL CLASSIFIER `beta`;\n" +
				"CREATE RESOURCE POOL CLASSIFIER `alpha` WITH (RESOURCE_POOL = 'batch', RANK = 20);\n" +
				"CREATE RESOURCE POOL CLASSIFIER `beta` WITH (RESOURCE_POOL = 'batch', RANK = 10);",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			planning, plan := workloadPlan(c, workloadCapabilities(), nil, workloadRequest(c, schema.ResourcePoolProperties, test.pools, nil),
				workloadRequest(c, schema.ResourcePoolClassifierProperties, test.classifiers, test.readClassifiers))
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, workloadplan.StepName)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, test.impact)
			var actions []protocol.PlanningEffect_Action
			for _, effect := range step.Metadata.Effects {
				actions = append(actions, effect.Action)
			}
			c.Assert(actions, qt.DeepEquals, test.actions)
			c.Assert(plan.Changes, qt.HasLen, len(planning.Changes))
			for _, receipt := range plan.Changes {
				c.Assert(receipt.Steps, qt.HasLen, 1)
				c.Assert(proto.Equal(receipt.Steps[0], step.Id), qt.IsTrue)
			}
			rendering := testplanning.RenderRequest(plan, workloadCapabilities())
			rendered, err := backend.New().Render(t.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(proto.Equal(rendered.Units[0].GetNative(), step.Operation.Value.Value), qt.IsTrue)
			rendering.Form = protocol.RenderRequest_TEXT
			text, err := backend.New().Render(t.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(text.Units, qt.HasLen, 1)
			c.Assert(text.Units[0].GetSql(), qt.Equals, test.text)
			normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
		})
	}
}

// A classifier names a user or a group, so the workload step runs after the
// common steps that create or change a role and before those that drop one.
func TestWorkloadPlanningOrdersRoles_HappyPath(t *testing.T) {
	c := qt.New(t)
	role := func(name string, action protocol.PlanningEffect_Action) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run", Name: "common/" + name},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "role",
				Effects: []*protocol.PlanningEffect{{Subject: &protocol.ComparisonIdentity{Kind: "role", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{},
					Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}, Action: action}}}}
	}
	common := []*protocol.CommonPlanningStep{role("etl", protocol.PlanningEffect_CREATE), role("old", protocol.PlanningEffect_DROP), role("reader", protocol.PlanningEffect_READ)}
	planning, plan := workloadPlan(c, workloadCapabilities(), common, workloadRequest(c, schema.ResourcePoolClassifierProperties, map[string]proto.Message{"etl": routing("default", 1)}, nil))
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
	var edges []string
	for _, edge := range plan.Contributions[0].Dependencies {
		edges = append(edges, edge.Before.Name+" -> "+edge.After.Name)
	}
	c.Assert(edges, qt.DeepEquals, []string{"common/etl -> " + workloadplan.StepName, workloadplan.StepName + " -> common/old"})
}

// A target without resource pools, or with transactional DDL, plans nothing
// and says why with the host's words.
func TestWorkloadPlanning_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities map[string]bool
		want         string
	}{
		{"no resource pools", map[string]bool{"transactional_ddl": false}, regexp.QuoteMeta("this target does not support resource pools and classifiers; " + workload.FlagHint)},
		{"DDL in transactions", map[string]bool{workload.CapabilityResourcePools: true, "transactional_ddl": true}, "Workload DDL cannot run in a SQL transaction.*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"batch": pool(workload.PoolSpec{})}, nil)
			_, plan := workloadPlan(c, test.capabilities, nil, request)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
		})
	}
}

// A pool and a classifier convert between their representations, both ways,
// keeping every setting: the host converts a read object it keeps into the
// desired representation.
func TestWorkloadConversion_HappyPath(t *testing.T) {
	c := qt.New(t)
	p := backend.New()
	read := &protocol.NormalizeValuesRequest{Engine: "ydb"}
	for _, value := range []struct {
		kind     string
		name     string
		settings proto.Message
	}{
		{schema.ResourcePoolProperties, "batch", pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0)), DatabaseLoadCPUThreshold: new(80.5)})},
		{schema.ResourcePoolClassifierProperties, "etl", schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: "batch", MemberName: "etl", Rank: 7})},
	} {
		envelope, err := schema.CaptureWorkloadObservation(&protocol.Reference{Engine: "ydb", Kind: value.kind, Database: "/local", Name: value.name}, value.settings)
		c.Assert(err, qt.IsNil)
		read.Values = append(read.Values, &protocol.ValueInput{Id: value.name, Representation: protocol.ValueInput_OBSERVED, Value: envelope})
	}
	normalized, err := p.NormalizeValues(c.Context(), read)
	c.Assert(err, qt.IsNil)
	request := &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: normalized.Values}
	desired, err := p.ConvertValues(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateConvertedValues(backend.Description(), request, desired), qt.IsNil)
	c.Assert(string(desired.Values[0].Value.Payload), qt.Equals, `{"concurrentQueryLimit":0,"databaseLoadCpuThreshold":80.5}`)
	c.Assert(string(desired.Values[1].Value.Payload), qt.Equals, `{"memberName":"etl","rank":"7","resourcePool":"batch"}`)
	back, err := p.ConvertValues(c.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: desired.Values})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(back, &protocol.NormalizedValues{Complete: true, Values: normalized.Values}), qt.IsTrue)
}

// A pool occupies no scheme path, so a table at the root of the same name is
// another object.
func TestWorkloadComparisonIgnoresRelations_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"items": pool(workload.PoolSpec{})}, nil)
	request.DeclaredRelations = []*protocol.ComparisonIdentity{{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{},
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("items")}}
	c.Assert(compareWorkload(c, request).Changes, qt.HasLen, 1)
}
