package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// readTopic is a topic as YDB 26.2.1.14 reports one made by
// `CREATE TOPIC app/events (CONSUMER worker) WITH (retention_period = Interval('P1D'))`.
func readTopic() *native.DescribeTopicResult {
	return &native.DescribeTopicResult{Self: &Ydb_Scheme.Entry{Owner: "root@builtin"},
		PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1, MaxActivePartitions: 1,
			AutoPartitioningSettings: &Ydb_Topic.AutoPartitioningSettings{Strategy: Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_DISABLED,
				PartitionWriteSpeed: &Ydb_Topic.AutoPartitioningWriteSpeedStrategy{StabilizationWindow: &durationpb.Duration{Seconds: 300}, UpUtilizationPercent: 80, DownUtilizationPercent: 20}}},
		RetentionPeriod: &durationpb.Duration{Seconds: 86400}, PartitionWriteSpeedBytesPerSecond: 1 << 20, PartitionWriteBurstBytes: 1 << 20,
		Attributes: map[string]string{"_timestamp_type": "CreateTime"},
		Consumers: []*native.Consumer{{Name: "worker", ReadFrom: &timestamppb.Timestamp{}, Attributes: map[string]string{"_service_type": "data-streams"},
			ConsumerType: &native.Consumer_StreamingConsumerType{StreamingConsumerType: &native.StreamingConsumerType{}}}}}
}

// topicRequest captures a comparison of one topic at app/events the way the
// host sends it: the declaration named without a database, the read topic
// and the read directory with the session's.
func topicRequest(c *qt.C, declared *ydbdecl.Topic, read *native.DescribeTopicResult) *protocol.CompareModelsRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}
	var observedModel *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == schema.TopicProperties {
			observedModel = model
		}
	}
	desiredState, currentState := testcomparison.State(schema.NativeDesiredModel(schema.TopicProperties)), testcomparison.State(observedModel)
	currentState.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	currentState.Coverage.Kinds[0].Sources[0].Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Targets: []string{"ydb"}, SubjectKinds: []string{schema.TopicProperties},
		Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""), Schema: testcomparison.Part("app")}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: topiccompare.ServiceID, Kinds: []string{schema.TopicProperties},
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"topics": true}, Desired: desiredState, Current: currentState}
	if declared != nil {
		intent, err := topics.Intent(*declared)
		c.Assert(err, qt.IsNil)
		value, err := schema.CaptureTopicIntent(owner, intent)
		c.Assert(err, qt.IsNil)
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared", Representation: protocol.ValueInput_DESIRED, Value: value}}})
		c.Assert(err, qt.IsNil)
		desiredState.Records = []*protocol.ComparisonRecord{{Kind: schema.TopicProperties, Subject: &protocol.ComparisonIdentity{Kind: schema.TopicProperties, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("events")}, Value: normalized.Values[0]}}
	}
	if read != nil {
		readOwner := proto.CloneOf(owner)
		readOwner.Database = "/local"
		value, err := schema.CaptureObject(readOwner, read)
		c.Assert(err, qt.IsNil)
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "read", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		currentState.Records = []*protocol.ComparisonRecord{{Kind: schema.TopicProperties, Subject: &protocol.ComparisonIdentity{Kind: schema.TopicProperties, Database: testcomparison.Part("/local"), Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("events")}, Value: normalized.Values[0]}}
	}
	return request
}

// A topic compares as the host compares one: settings resolved to the values
// YDB gives a new topic, consumers by name, and YDB's attributes and access
// left out. Each change says what it costs.
func TestTopicComparison_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared *ydbdecl.Topic
		read     *native.DescribeTopicResult
		changes  int
		impact   protocol.ChangeSemantics_Impact
		removes  bool
	}{
		{name: "the same topic", declared: &ydbdecl.Topic{RetentionPeriod: "PT24H", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, read: readTopic()},
		{name: "a longer retention", declared: &ydbdecl.Topic{RetentionPeriod: "P2D", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, read: readTopic(), changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "a consumer added", declared: &ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker"}, {Name: "audit"}}}, read: readTopic(), changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "a consumer dropped", declared: &ydbdecl.Topic{}, read: readTopic(), changes: 1, impact: protocol.ChangeSemantics_DESTRUCTIVE, removes: true},
		{name: "a consumer changed", declared: &ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker", Important: true}}}, read: readTopic(), changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "a creation", declared: &ydbdecl.Topic{}, changes: 1, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "a removal", read: readTopic(), changes: 1, impact: protocol.ChangeSemantics_DESTRUCTIVE, removes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := topicRequest(c, test.declared, test.read)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			for _, change := range result.Changes {
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, test.impact)
				c.Assert(change.Value.ChangeSemantics.GetRemovesState(), qt.Equals, test.removes)
				normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: change.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: change.Value.Value}}})
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(normalized.Values[0], change.Value), qt.IsTrue)
			}
		})
	}
}

// A topic the desired source does not describe is kept as YDB holds it.
func TestTopicComparisonKeepsAnUndescribedTopic_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := topicRequest(c, nil, readTopic())
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Desired.Records, qt.HasLen, 1)
	kept, err := schema.DesiredTopic(result.Desired.Records[0].Value.Value)
	c.Assert(err, qt.IsNil)
	spec, err := topics.FromIntent(kept)
	c.Assert(err, qt.IsNil)
	c.Assert(spec.Consumers, qt.DeepEquals, []ydbdecl.Consumer{{Name: "worker"}})
}

// A read topic holding a setting no declaration states refuses the
// comparison with the host's words, whatever the declaration says.
func TestTopicComparison_FailurePath(t *testing.T) {
	c := qt.New(t)
	read := readTopic()
	read.RetentionStorageMb = 64
	for _, declared := range []*ydbdecl.Topic{nil, {}} {
		result, err := backend.New().CompareModels(t.Context(), topicRequest(c, declared, read))
		c.Assert(err, qt.ErrorMatches, `.*it holds retention_storage_mb, which Ptah does not model and no YQL statement sets.*`)
		c.Assert(result, qt.IsNil)
	}
}

// topicPlan compares and plans one topic change.
func topicPlan(c *qt.C, declared *ydbdecl.Topic, read *native.DescribeTopicResult, capabilities map[string]bool) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := topicRequest(c, declared, read)
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: topicplan.ServiceID, Identifiers: request.Identifiers, Capabilities: capabilities,
		Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}}
	plan, err := backend.New().PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	return planning, plan
}

// A topic change becomes one native step that forbids a transaction, whose
// operation renders as a native unit and normalizes back to itself.
func TestTopicPlanning_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared *ydbdecl.Topic
		read     *native.DescribeTopicResult
		action   protocol.PlanningEffect_Action
	}{
		{"create", &ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker", AvailabilityPeriod: "PT2H"}}}, nil, protocol.PlanningEffect_CREATE},
		{"alter", &ydbdecl.Topic{RetentionPeriod: "P2D"}, readTopic(), protocol.PlanningEffect_ALTER},
		{"drop", nil, readTopic(), protocol.PlanningEffect_DROP},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			capabilities := map[string]bool{"topics": true, "topic_consumer_availability_period": true, "transactional_ddl": false}
			planning, plan := topicPlan(c, test.declared, test.read, capabilities)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, test.action)
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, schema.TopicOperation)
			rendering := testplanning.RenderRequest(plan, capabilities)
			rendered, err := backend.New().Render(t.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(proto.Equal(rendered.Units[0].GetNative(), step.Operation.Value.Value), qt.IsTrue)
			normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
		})
	}
}

// A change YDB cannot make in place, or one the target lacks a capability
// for, is refused with the host's words and plans nothing.
func TestTopicPlanning_FailurePath(t *testing.T) {
	wide := readTopic()
	wide.PartitioningSettings.MinActivePartitions = 3
	for _, test := range []struct {
		name         string
		declared     *ydbdecl.Topic
		read         *native.DescribeTopicResult
		capabilities map[string]bool
		want         string
	}{
		{"fewer partitions", &ydbdecl.Topic{MinActivePartitions: 2, Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, wide, map[string]bool{"topics": true, "transactional_ddl": false},
			"topic app/events: it has 3 partitions and is declared with 2, .*"},
		{"an availability period on 25.3", &ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker", AvailabilityPeriod: "PT2H"}}}, readTopic(), map[string]bool{"topics": true, "transactional_ddl": false},
			`consumer "worker" of topic app/events takes availability_period, which requires target capability topic_consumer_availability_period, unavailable on this ydb target`},
		{"no topics", &ydbdecl.Topic{}, nil, map[string]bool{"transactional_ddl": false}, "Planning a topic requires the topics capability and nontransactional DDL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, plan := topicPlan(c, test.declared, test.read, test.capabilities)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
		})
	}
}
