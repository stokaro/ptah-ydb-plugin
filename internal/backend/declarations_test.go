package backend_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

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

// dependencyNames lists the dependencies of contribution as `before ->
// after`, each step named by the object it creates.
func dependencyNames(contribution *protocol.PlanningContribution) []string {
	names := make(map[string]string)
	for _, step := range contribution.Steps {
		names[step.Id.Name] = step.Operation.Value.Value.Owner.Name
	}
	var listed []string
	for _, edge := range contribution.Dependencies {
		listed = append(listed, names[edge.Before.Name]+" -> "+names[edge.After.Name])
	}
	return listed
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
	c.Assert(dependencyNames(result.Contributions[0]), qt.DeepEquals, []string{"store -> external", "batch.pool -> routing"})
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
// the query that YDB compiles against them. A text may name them by absolute
// path below the root its value names paths in, the database `schema inspect`
// read it from: YDB holds a query's text as written, and the render keeps it
// so. A query that names no topic it can resolve is refused before anything
// is planned, and so is one on a target without streaming queries.
func TestAuthoredStreamingQueryFollowsItsTopics(t *testing.T) {
	for _, test := range []struct {
		name, text string
		context    *protocol.DatabaseContext
	}{
		{name: "relative names", text: "INSERT INTO `app/output` SELECT * FROM `app/input`;"},
		{name: "absolute names below the database the query was read from", text: "INSERT INTO `/local/app/output` SELECT * FROM `/local/app/input`;",
			context: &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}},
	} {
		t.Run(test.name, func(t *testing.T) {
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
				streaming.Spec{Text: test.text}, false)
			c.Assert(err, qt.IsNil)
			query.Context = proto.CloneOf(test.context)
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
			c.Assert(dependencyNames(result.Contributions[0]), qt.DeepEquals, []string{"output -> copy", "input -> copy"})
			rendering := testplanning.RenderRequest(backend.Description(), &protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
			rendering.TargetFeatures, rendering.Form = request.TargetFeatures, protocol.RenderRequest_TEXT
			text, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(text.Units[0].GetSql(), qt.Equals, streaming.Create("app", "copy", streaming.Spec{Text: test.text}))
		})
	}
}

func TestAuthoredStreamingQuery_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, text string
		context    *protocol.DatabaseContext
		capable    bool
		want       string
	}{
		{name: "a target without streaming queries", text: "INSERT INTO `app/output` SELECT * FROM `app/input`;",
			want: `app/copy: requires target capability streaming_queries .*`},
		{name: "an absolute path with no root to read it below", text: "INSERT INTO `/local/app/output` SELECT * FROM `app/input`;", capable: true,
			want: `streaming query path is outside the captured database or dev realm`},
		{name: "an absolute path outside the database the query was read from", text: "INSERT INTO `/other/app/output` SELECT * FROM `app/input`;",
			context: &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}, capable: true,
			want: `streaming query path is outside the captured database or dev realm`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"},
				streaming.Spec{Text: test.text}, false)
			c.Assert(err, qt.IsNil)
			query.Context = proto.CloneOf(test.context)
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

// pathFixtures are authored objects the path tests declare, each by the
// object it is and the path it takes.
func pathFixtures(c *qt.C) map[string]*protocol.Envelope {
	c.Helper()
	at := func(kind, directory, name string) *protocol.Reference {
		return &protocol.Reference{Engine: "ydb", Kind: kind, Schema: directory, Name: name}
	}
	secret, err := schema.CaptureSecretIntent(at(schema.SecretProperties, "ext", "pw"), &native.SecretIntent{ValueEnv: "PTAH_SECRET_PW"})
	c.Assert(err, qt.IsNil)
	topicIntent, err := topics.Intent(ydbdecl.Topic{})
	c.Assert(err, qt.IsNil)
	topic, err := schema.CaptureTopicIntent(at(schema.TopicProperties, "app", "events"), topicIntent)
	c.Assert(err, qt.IsNil)
	sourceIntent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://example.com/bucket", AuthMethod: "NONE"})
	c.Assert(err, qt.IsNil)
	source, err := schema.CaptureExternalIntent(at(schema.ExternalSourceProperties, "app", "notes"), sourceIntent)
	c.Assert(err, qt.IsNil)
	bucket, err := schema.CaptureExternalIntent(at(schema.ExternalSourceProperties, "ext", "bucket"), sourceIntent)
	c.Assert(err, qt.IsNil)
	tableIntent, err := externals.TableIntent(ydbdecl.ExternalTable{DataSource: "ext/bucket", Location: "events/", Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Uint64"}}})
	c.Assert(err, qt.IsNil)
	table, err := schema.CaptureExternalIntent(at(schema.ExternalTableProperties, "ext", "pw"), tableIntent)
	c.Assert(err, qt.IsNil)
	return map[string]*protocol.Envelope{"secret ext/pw": secret, "topic app/events": topic, "data source app/notes": source,
		"data source ext/bucket": bucket, "external table ext/pw": table}
}

// declaredTableAt is the table the schema declares at directory/name, as the
// host hands a declared table to planning: a key and no features.
func declaredTableAt(directory, name string) *protocol.ValidationTable {
	empty := func() *protocol.ComparisonState {
		return &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}
	}
	return &protocol.ValidationTable{Subject: &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part(directory), Name: testcomparison.Part(name)},
		Declaration: &protocol.DeclaredTable{Schema: directory, Name: name, PrimaryKey: []string{"id"}, Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}},
			Features: &protocol.CapturedFeatures{Objects: empty(), Facets: empty()}}}
}

// An authored object at the path of a table or a view the same schema
// declares, or at the path of another authored object, is refused before
// anything is planned, naming the object's model: YDB keeps one object at a
// path (`unexpected path type`). A view reaches the request as the common
// step that creates it. Ported from the host's
// TestSecretDeclarations_RefusesAPathATableTakes,
// TestTopicDeclarations_RefusesAPathATableTakes, TestRender_Topic_FailurePath,
// TestRender_External_FailurePath and
// TestValidateSchema_YDBRefusesACoordinationNodeOnAnotherObjectsPath.
func TestAuthoredObjectAtATakenPath_FailurePath(t *testing.T) {
	fixtures := pathFixtures(testcheck.New(t))
	for _, test := range []struct {
		name    string
		objects []string
		tables  []*protocol.ValidationTable
		common  []*protocol.CommonPlanningStep
		kind    string
		want    string
	}{
		{name: "a topic at a declared view's path", objects: []string{"topic app/events"}, common: []*protocol.CommonPlanningStep{commonView("events", createView)},
			kind: schema.TopicProperties, want: "authored native object conflicts with a declared view path"},
		{name: "a secret at a declared table's path", objects: []string{"secret ext/pw"}, tables: []*protocol.ValidationTable{declaredTableAt("ext", "pw")},
			kind: schema.SecretProperties, want: "authored native object conflicts with a declared table path"},
		{name: "a topic at a declared table's path", objects: []string{"topic app/events"}, tables: []*protocol.ValidationTable{declaredTableAt("app", "events")},
			kind: schema.TopicProperties, want: "authored native object conflicts with a declared table path"},
		{name: "a data source at a declared table's path", objects: []string{"data source app/notes"}, tables: []*protocol.ValidationTable{declaredTableAt("app", "notes")},
			kind: schema.ExternalSourceProperties, want: "authored native object conflicts with a declared table path"},
		{name: "an external table at a declared secret's path", objects: []string{"secret ext/pw", "data source ext/bucket", "external table ext/pw"},
			kind: schema.ExternalTableProperties, want: "authored native objects share or contain one scheme path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			var values []*protocol.Envelope
			for _, name := range test.objects {
				values = append(values, fixtures[name])
			}
			request := declarationRequest(c, values...)
			request.Capabilities["secrets"], request.Capabilities["external_data_source_secret_paths"] = true, true
			request.Tables = test.tables
			request.CommonSteps = test.common

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].Problem.Kind, qt.Equals, test.kind)
			c.Assert(result.Diagnostics[0].Problem.Message, qt.Equals, test.want)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Declarations, qt.HasLen, 0)
		})
	}
}

// The control: the same objects beside a table and a view at other paths,
// and each on its own path, are planned, and so is an object beside a view
// the plan drops at its path.
func TestAuthoredObjectBesideATableAtAnotherPath_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fixtures := pathFixtures(c)
	request := declarationRequest(c, fixtures["secret ext/pw"], fixtures["topic app/events"], fixtures["data source app/notes"])
	request.Capabilities["secrets"] = true
	request.Tables = []*protocol.ValidationTable{declaredTableAt("app", "other")}
	dropped := commonView("notes", dropView)
	dropped.Id.Name = "dropped view"
	request.CommonSteps = []*protocol.CommonPlanningStep{commonView("eventsview", createView), dropped}

	result, err := backend.New().PlanDeclarations(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Declarations, qt.HasLen, 3)
}

// A refused declaration names the object by its index in the request, not by
// its place in the order the planner reads the objects in, which sorts them by
// identity. Ported from the host's
// TestWorkloadDeclarationRefusalKeepsOriginalObjectIndex.
func TestAuthoredDeclarationRefusalNamesTheObjectsIndex_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := declarationRequest(c, declaredNode(c, "zeta", nil), declaredNode(c, "middle", nil), declaredNode(c, "alpha", &native.AccessIntent{Owner: new("user")}))

	result, err := backend.New().PlanDeclarations(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetObject(), qt.Equals, uint32(2))
	c.Assert(result.Diagnostics[0].Problem.Kind, qt.Equals, schema.CoordinationProperties)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Declarations, qt.HasLen, 0)
}
