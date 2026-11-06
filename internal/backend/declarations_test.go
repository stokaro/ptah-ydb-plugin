package backend_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func declarationRequest(c *qt.C, values ...*protocol.Envelope) *protocol.PlanDeclarationsRequest {
	c.Helper()
	description := backend.Description()
	request := &protocol.PlanDeclarationsRequest{Engine: "ydb", DeclarationId: declarationplan.ServiceID, TargetFeatures: description.Engines[0].Features, Identifiers: &protocol.ComparisonIdentifiers{Rules: description.Engines[0].Identifiers}, Capabilities: map[string]bool{"transactional_ddl": false, "coordination_nodes": true, "topics": true, "external_data_sources": true, "resource_pools": true}}
	for i, value := range values {
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: request.TargetFeatures, Values: []*protocol.ValueInput{{Id: strconv.Itoa(i), Value: value, Representation: protocol.ValueInput_DESIRED}}})
		c.Assert(err, qt.IsNil)
		request.Objects = append(request.Objects, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}
	return request
}
func declaredNode(c *qt.C, name string, access *native.AccessIntent) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureCoordinationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: "app", Name: name}, schema.CoordinationIntent{SelfCheckPeriodMillis: new(uint32(2000)), Access: access})
	c.Assert(err, qt.IsNil)
	return value
}

func TestAuthoredDeclarationsCreateEveryNativeFamily(t *testing.T) {
	c := testcheck.New(t)
	node := declaredNode(c, "node", nil)
	topicIntent, err := topics.Intent(ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker"}}, RetentionPeriod: "P1D"})
	c.Assert(err, qt.IsNil)
	topic, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}, topicIntent)
	c.Assert(err, qt.IsNil)
	sourceIntent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://example.com/bucket", AuthMethod: "NONE"})
	c.Assert(err, qt.IsNil)
	source, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ExternalSourceProperties, Schema: "app", Name: "store"}, sourceIntent)
	c.Assert(err, qt.IsNil)
	tableIntent, err := externals.TableIntent(ydbdecl.ExternalTable{DataSource: "app/store", Location: "data/", Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Uint64"}}})
	c.Assert(err, qt.IsNil)
	table, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ExternalTableProperties, Schema: "app", Name: "external"}, tableIntent)
	c.Assert(err, qt.IsNil)
	pool, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: "batch.pool"}, schema.PoolSettings(workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}))
	c.Assert(err, qt.IsNil)
	classifier, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: "routing"}, schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: "batch.pool", Rank: 100}))
	c.Assert(err, qt.IsNil)
	request := declarationRequest(c, node, topic, table, source, classifier, pool)
	result, err := backend.New().PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Declarations, qt.HasLen, 6)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	rendering := testplanning.RenderRequest(backend.Description(), &protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
	rendering.TargetFeatures = request.TargetFeatures
	rendering.Form = protocol.RenderRequest_TEXT
	text, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(text.Units, qt.HasLen, 6)
	byName := make(map[string]string)
	for i, step := range result.Contributions[0].Steps {
		byName[step.Operation.Value.Value.Owner.Name] = text.Units[i].GetSql()
		var payload map[string]json.RawMessage
		c.Assert(json.Unmarshal(step.Operation.Value.Value.Payload, &payload), qt.IsNil)
		c.Assert(payload, qt.HasLen, 1)
		c.Assert(payload["desired"], qt.IsNotNil)
	}
	c.Assert(byName["node"], qt.Equals, "CREATE COORDINATION NODE `app/node` WITH (self_check_period = Interval('PT2S'));")
	c.Assert(byName["events"], qt.Contains, "CREATE TOPIC `app/events`")
	c.Assert(byName["events"], qt.Contains, "CONSUMER `worker`")
	c.Assert(byName["external"], qt.Contains, "CREATE EXTERNAL TABLE `app/external`")
	c.Assert(byName["store"], qt.Contains, "CREATE EXTERNAL DATA SOURCE `app/store`")
	c.Assert(byName["batch.pool"], qt.Contains, "CREATE RESOURCE POOL `batch.pool`")
	c.Assert(byName["routing"], qt.Contains, "CREATE RESOURCE POOL CLASSIFIER `routing`")
	for _, step := range result.Contributions[0].Steps {
		if step.Operation.Value.Value.Owner.Name == "external" || step.Operation.Value.Value.Owner.Name == "routing" {
			c.Assert(step.Metadata.Effects[len(step.Metadata.Effects)-1].Action, qt.Equals, protocol.PlanningEffect_READ)
		}
	}
	rendering.Form = protocol.RenderRequest_ANY
	executable, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(text, executable), qt.IsTrue)
}

func TestAuthoredDeclarationRefusalDiscardsEveryPrefix(t *testing.T) {
	c := testcheck.New(t)
	private := declarationRequest(c, declaredNode(c, "private", &native.AccessIntent{Owner: new("user")})).Objects[0]
	child := declaredNode(c, "child", nil)
	child.Owner.Schema = "app/node"
	collision := declarationRequest(c, child).Objects[0]
	for _, test := range []struct {
		name string
		edit func(*protocol.PlanDeclarationsRequest)
	}{
		{"unknown capability", func(r *protocol.PlanDeclarationsRequest) { delete(r.Capabilities, "coordination_nodes") }},
		{"access requirement", func(r *protocol.PlanDeclarationsRequest) { r.Objects = append(r.Objects, private) }},
		{"native path collision", func(r *protocol.PlanDeclarationsRequest) { r.Objects = append(r.Objects, collision) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := declarationRequest(c, declaredNode(c, "node", nil))
			test.edit(request)
			for i, object := range request.Objects {
				object.Value.Id = strconv.Itoa(i)
			}
			result, err := backend.New().PlanDeclarations(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Declarations, qt.HasLen, 0)
		})
	}
}

func TestAuthoredDeclarationRejectsForgedReceiptAndCancellation(t *testing.T) {
	c := testcheck.New(t)
	request := declarationRequest(c, declaredNode(c, "node", nil))
	request.Objects[0].Value.CanonicalProjection = []byte(`{}`)
	result, err := backend.New().PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err = backend.New().PlanDeclarations(ctx, request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

// An authored streaming query is created with its own CREATE STREAMING QUERY,
// and its step reads the scheme slot of each topic its text reads or writes,
// which the topic's own creation makes, so a plan creates both topics before
// the query that YDB compiles against them. A query that names no topic it
// can resolve is refused before anything is planned, and so is one on a
// target without streaming queries.
func TestAuthoredStreamingQueryFollowsItsTopics(t *testing.T) {
	c := testcheck.New(t)
	var topicValues []*protocol.Envelope
	for _, name := range []string{"input", "output"} {
		intent, err := topics.Intent(ydbdecl.Topic{})
		c.Assert(err, qt.IsNil)
		value, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: name}, intent)
		c.Assert(err, qt.IsNil)
		topicValues = append(topicValues, value)
	}
	query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"},
		streaming.Spec{Text: "INSERT INTO `app/output` SELECT * FROM `app/input`;"}, false)
	c.Assert(err, qt.IsNil)
	request := declarationRequest(c, query, topicValues[0], topicValues[1])
	request.Capabilities[streaming.Capability] = true

	result, err := backend.New().PlanDeclarations(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	effects := make(map[string][]string)
	for _, step := range result.Contributions[0].Steps {
		owner := step.Operation.Value.Value.Owner.Name
		for _, effect := range step.Metadata.Effects {
			effects[owner] = append(effects[owner], effect.Action.String()+" "+effect.Subject.Kind+" "+effect.Subject.Schema.Normalized+"/"+effect.Subject.Name.Normalized)
		}
	}
	c.Assert(effects, qt.DeepEquals, map[string][]string{
		"copy": {"CREATE " + schema.StreamingQueryProperties + " app/copy", "CREATE ydb.scheme-path app/copy",
			"READ ydb.scheme-path app/output", "READ ydb.scheme-path app/input"},
		"input":  {"CREATE " + schema.TopicProperties + " app/input", "CREATE ydb.scheme-path app/input"},
		"output": {"CREATE " + schema.TopicProperties + " app/output", "CREATE ydb.scheme-path app/output"},
	})
	rendering := testplanning.RenderRequest(backend.Description(), &protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
	rendering.TargetFeatures, rendering.Form = request.TargetFeatures, protocol.RenderRequest_TEXT
	text, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(text.Units[0].GetSql(), qt.Equals, streaming.Create("app", "copy", streaming.Spec{Text: "INSERT INTO `app/output` SELECT * FROM `app/input`;"}))
}

func TestAuthoredStreamingQuery_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, text string
		capable    bool
		want       string
	}{
		{name: "a target without streaming queries", text: "INSERT INTO `app/output` SELECT * FROM `app/input`;",
			want: `app/copy: requires target capability streaming_queries .*`},
		{name: "an absolute path an authored declaration cannot place", text: "INSERT INTO `/local/app/output` SELECT * FROM `app/input`;", capable: true,
			want: `streaming query path is outside the captured database or dev realm`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"},
				streaming.Spec{Text: test.text}, false)
			c.Assert(err, qt.IsNil)
			request := declarationRequest(c, query)
			request.Capabilities[streaming.Capability] = test.capable

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].Problem.Message, qt.Matches, test.want)
		})
	}
}

// Every authored creation asks for the lifecycle span where the host
// negotiated it, so a schema render writes it as soon as what it reads
// exists, as the host's embedded engine at 74de0b65b wrote a topic ahead of
// the tables, and for the default window, after the tables, otherwise.
func TestAuthoredDeclarationsAskForTheLifecycleSpan_HappyPath(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle bool
		want      protocol.PlanningOperation_Phase
	}{
		{name: "the host negotiated planning.lifecycle", lifecycle: true, want: protocol.PlanningOperation_LIFECYCLE},
		{name: "the host did not", lifecycle: false, want: protocol.PlanningOperation_DEFAULT},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			topicIntent, err := topics.Intent(ydbdecl.Topic{RetentionPeriod: "P1D"})
			c.Assert(err, qt.IsNil)
			topic, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}, topicIntent)
			c.Assert(err, qt.IsNil)
			request := declarationRequest(c, declaredNode(c, "node", nil), topic)
			request.TargetFeatures = slices.DeleteFunc(slices.Clone(request.TargetFeatures), func(feature string) bool {
				return !test.lifecycle && feature == provider.PlanningLifecycleFeature
			})

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
			var phases []protocol.PlanningOperation_Phase
			for _, step := range result.Contributions[0].Steps {
				phases = append(phases, step.Operation.Phase)
			}
			c.Assert(phases, qt.DeepEquals, []protocol.PlanningOperation_Phase{test.want, test.want})
		})
	}
}

// A coordination node, a resource pool, a classifier and a streaming query
// follow the common steps, up to the first grant or revoke on an object the
// request declares, as the host's embedded engine at 74de0b65b wrote them
// after the common objects and before the grants on the objects it created. A
// topic, a secret and the external objects keep no such dependency: that
// engine wrote them first.
func TestAuthoredLateKindsFollowTheCommonSteps_HappyPath(t *testing.T) {
	role := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "role"}, Metadata: &protocol.PlanningMetadata{
		Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Create a role",
		Effects: []*protocol.PlanningEffect{{Subject: &protocol.ComparisonIdentity{Kind: "role", Name: testcomparison.Part("readers")}, Action: protocol.PlanningEffect_CREATE}},
	}}
	marker := func(name string) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: name}, Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN}}
	}
	grant := func(name, kind string, subject *protocol.ComparisonIdentity) *protocol.CommonPlanningStep {
		step := commonAccessFor(subject, kind, grantAccess)
		step.Id = &protocol.StepIdentity{Owner: "host", Name: name}
		return step
	}
	table := func(name string) *protocol.ComparisonIdentity {
		return &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part("app"), Name: testcomparison.Part(name)}
	}
	tests := []struct {
		name      string
		lifecycle bool
		// common names the steps of the host: role, a marker, a grant on a
		// table t or u, on the node, on the topic or on the database.
		common []string
		want   []string
	}{
		{name: "a grant on the node ends the run", lifecycle: true, common: []string{"role", "marker", "grant t", "grant node", "grant u", "end"}, want: []string{"grant t -> node"}},
		{name: "a grant on the topic ends it too", lifecycle: true, common: []string{"role", "grant topic", "grant t"}, want: []string{"role -> node"}},
		{name: "no grant on a declared object", lifecycle: true, common: []string{"role", "grant t", "grant u", "end"}, want: []string{"grant u -> node"}},
		{name: "a grant on the database, which has no effects", lifecycle: true, common: []string{"role", "grant t", "grant database", "end"}, want: []string{"grant database -> node"}},
		{name: "no common step", lifecycle: true},
		{name: "the host did not negotiate planning.lifecycle", common: []string{"role", "grant t"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			topicIntent, err := topics.Intent(ydbdecl.Topic{RetentionPeriod: "P1D"})
			c.Assert(err, qt.IsNil)
			topic, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}, topicIntent)
			c.Assert(err, qt.IsNil)
			request := declarationRequest(c, declaredNode(c, "node", nil), topic)
			request.TargetFeatures = slices.DeleteFunc(slices.Clone(request.TargetFeatures), func(feature string) bool {
				return !test.lifecycle && feature == provider.PlanningLifecycleFeature
			})
			steps := map[string]*protocol.CommonPlanningStep{
				"role": role, "marker": marker("marker"), "end": marker("end"),
				"grant t": grant("grant t", "table", table("t")), "grant u": grant("grant u", "table", table("u")),
				"grant node":  grant("grant node", "coordination node", request.Objects[0].Subject),
				"grant topic": grant("grant topic", "topic", request.Objects[1].Subject),
				"grant database": {Id: &protocol.StepIdentity{Owner: "host", Name: "grant database"}, Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN},
					CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "readers", Privileges: []string{"ydb.database.connect"},
						Target: &protocol.Reference{Engine: "ydb", Kind: "database", Name: "/local"}}}}},
			}
			for _, name := range test.common {
				request.CommonSteps = append(request.CommonSteps, steps[name])
			}

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
			declared := make(map[string]string)
			for _, step := range result.Contributions[0].Steps {
				declared[step.Id.Name] = step.Operation.Value.Value.Owner.Name
			}
			var dependencies []string
			for _, dependency := range result.Contributions[0].Dependencies {
				dependencies = append(dependencies, dependency.Before.Name+" -> "+declared[dependency.After.Name])
			}
			c.Assert(dependencies, qt.DeepEquals, test.want)
		})
	}
}

// defaultPool is the pool default declared with spec.
func defaultPool(c *qt.C, spec workload.PoolSpec) *protocol.Envelope {
	c.Helper()
	pool, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: workload.DefaultPool}, schema.PoolSettings(spec))
	c.Assert(err, qt.IsNil)
	return pool
}

// The pool default that names no setting is written as nothing, as the host's
// embedded engine at 74de0b65b wrote it: YDB creates it with the database. Its
// receipt names no step, and a classifier that reads it still plans.
func TestAuthoredDefaultPoolThatSetsNothingWritesNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	classifier, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: "routing"},
		schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: workload.DefaultPool, Rank: 100}))
	c.Assert(err, qt.IsNil)
	request := declarationRequest(c, defaultPool(c, workload.PoolSpec{}), classifier)

	result, err := backend.New().PlanDeclarations(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Declarations[0].Steps, qt.HasLen, 0)
	c.Assert(result.Declarations[1].Steps, qt.HasLen, 1)
	c.Assert(result.Contributions, qt.HasLen, 1)
	c.Assert(result.Contributions[0].Steps, qt.HasLen, 1)
	c.Assert(result.Contributions[0].Steps[0].Operation.Value.Value.Owner.Name, qt.Equals, "routing")
}

// The pool default that names a setting is written as the ALTER RESOURCE POOL
// ... SET of it, as the host's embedded engine at 74de0b65b wrote it, where
// CREATE RESOURCE POOL fails on the pool the database holds. The step alters
// the pool, so a classifier's read of it follows.
func TestAuthoredDefaultPoolSetsWhatItNames_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := declarationRequest(c, defaultPool(c, workload.PoolSpec{QueryMemoryLimitPercentPerNode: new(float64(50))}))

	result, err := backend.New().PlanDeclarations(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 1)
	step := result.Contributions[0].Steps[0]
	c.Assert(step.Metadata.Effects, qt.HasLen, 1)
	c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_ALTER)
	c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	rendering := testplanning.RenderRequest(backend.Description(), &protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
	rendering.TargetFeatures = request.TargetFeatures
	rendering.Form = protocol.RenderRequest_TEXT
	text, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(text.Units, qt.HasLen, 1)
	c.Assert(text.Units[0].GetSql(), qt.Equals, "ALTER RESOURCE POOL `default` SET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 50);")
}
