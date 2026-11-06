package backend_test

import (
	"context"
	"encoding/json"
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
	c.Assert(dependencyNames(result.Contributions[0]), qt.DeepEquals, []string{"output -> copy", "input -> copy"})
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
