package backend_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	request := &protocol.CompareModelsRequest{Engine: "ydb", DatabasePath: "/local", ComparisonId: topiccompare.ServiceID, Kinds: []string{schema.TopicProperties},
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
			c := testcheck.New(t)
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
	c := testcheck.New(t)
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

// unreadableTopics are read topics no declaration can describe, each with the
// reason the host's embedded engine refused to read it with.
func unreadableTopics() []struct {
	name, why string
	read      *native.DescribeTopicResult
} {
	storage, codec := readTopic(), readTopic()
	storage.RetentionStorageMb = 64
	codec.Consumers = append(codec.Consumers, &native.Consumer{Name: "custom", SupportedCodecs: &Ydb_Topic.SupportedCodecs{Codecs: []int32{10042}}})
	return []struct {
		name, why string
		read      *native.DescribeTopicResult
	}{
		{name: "an unmodeled setting", why: "it holds retention_storage_mb, which Ptah does not model and no YQL statement sets; read a schema that does not hold the topic", read: storage},
		{name: "a consumer's codec", why: `consumer "custom": its codec list names codec 10042, which Ptah does not declare`, read: codec},
	}
}

// A read topic holding a setting no declaration states refuses the
// comparison with the host's words, whatever the declaration says, and a
// source that does not describe the topic too: `YDB topic <path>: <why>`, as
// a reason the host prints.
func TestTopicComparison_FailurePath(t *testing.T) {
	for _, topic := range unreadableTopics() {
		for _, test := range []struct {
			name     string
			declared *ydbdecl.Topic
			outside  protocol.ModelKnowledge_State
		}{
			{name: "no declaration", outside: protocol.ModelKnowledge_COMPLETE},
			{name: "a declaration", declared: &ydbdecl.Topic{}, outside: protocol.ModelKnowledge_COMPLETE},
			{name: "a source that does not describe it", outside: protocol.ModelKnowledge_UNINSPECTED},
		} {
			t.Run(topic.name+"/"+test.name, func(t *testing.T) {
				c := testcheck.New(t)
				request := topicRequest(c, test.declared, topic.read)
				request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(test.outside)

				result, err := backend.New().CompareModels(t.Context(), request)

				c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = `+regexp.QuoteMeta("YDB topic app/events: "+topic.why))
				c.Assert(statusReasons(c, err), qt.DeepEquals, []string{"YDB topic app/events: " + topic.why})
				c.Assert(result, qt.IsNil)
			})
		}
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
			c := testcheck.New(t)
			capabilities := map[string]bool{"topics": true, "topic_consumer_availability_period": true, "transactional_ddl": false}
			planning, plan := topicPlan(c, test.declared, test.read, capabilities)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, test.action)
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, schema.TopicOperation)
			rendering := testplanning.RenderRequest(backend.Description(), plan, capabilities)
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

// In the TEXT form a topic change is the statements the host's embedded
// engine wrote into a migration file for it, which are the ones Execute runs
// natively: one CREATE TOPIC, one ALTER TOPIC, one DROP TOPIC.
func TestTopicTextForm_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared *ydbdecl.Topic
		read     *native.DescribeTopicResult
		want     string
	}{
		{"create", &ydbdecl.Topic{MinActivePartitions: 1, Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, nil,
			"CREATE TOPIC `app/events` (CONSUMER `worker`) WITH (min_active_partitions = 1);"},
		{"alter", &ydbdecl.Topic{RetentionPeriod: "P2D", Consumers: []ydbdecl.Consumer{{Name: "worker"}, {Name: "audit", Important: true}}}, readTopic(),
			"ALTER TOPIC `app/events` SET (min_active_partitions = 1, auto_partitioning_strategy = 'disabled', retention_period = Interval('P2D'), " +
				"partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576, supported_codecs = ''), ADD CONSUMER `audit` WITH (important = TRUE);"},
		{"drop", nil, readTopic(), "DROP TOPIC `app/events`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			capabilities := map[string]bool{"topics": true, "topic_consumer_availability_period": true, "transactional_ddl": false}
			_, plan := topicPlan(c, test.declared, test.read, capabilities)
			rendering := testplanning.RenderRequest(backend.Description(), plan, capabilities)
			rendering.Form = protocol.RenderRequest_TEXT

			rendered, err := backend.New().Render(t.Context(), rendering)

			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Refusal, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(rendered.Units[0].GetSql(), qt.Equals, test.want)
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
			c := testcheck.New(t)
			_, plan := topicPlan(c, test.declared, test.read, test.capabilities)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
		})
	}
}

// readByTransfer is readTopic with its consumer worker marked as the one a
// transfer of the read reads the topic through.
func readByTransfer() *native.DescribeTopicResult {
	read := readTopic()
	read.Consumers[0].Transfer = "app/ingest"
	return read
}

// A declaration that leaves out the consumer a transfer reads the topic
// through keeps it: the consumer is the transfer's, and dropping it under the
// running transfer is what the comparison would plan otherwise.
func TestTopicComparisonKeepsATransfersConsumer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := topicRequest(c, &ydbdecl.Topic{RetentionPeriod: "PT24H"}, readByTransfer())

	result, err := backend.New().CompareModels(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
}

// A change of a topic a transfer reads keeps the transfer's consumer the
// declaration leaves out: the change's declared side holds it, so the plan
// neither drops it nor reports the change as destructive.
func TestTopicChangeKeepsATransfersConsumer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := topicRequest(c, &ydbdecl.Topic{RetentionPeriod: "P2D"}, readByTransfer())

	result, err := backend.New().CompareModels(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(result.Changes[0].Value.ChangeSemantics.GetRemovesState(), qt.IsFalse)
	_, desired, err := schema.TopicSpecs(result.Changes[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(desired.Consumers, qt.DeepEquals, []ydbdecl.Consumer{{Name: "worker"}})
	c.Assert(desired.RetentionPeriod, qt.Equals, "P2D")
}

// A consumer whose change YDB makes only by dropping it and adding it again,
// a declaration that takes any codec where the read consumer lists some,
// loses its position in the topic, so the change is destructive. Ported from
// the host's TestTopicChange_Effect and TestClassify_Topic.
func TestTopicComparisonReAddedConsumerIsDestructive_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	read := readTopic()
	read.Consumers[0].SupportedCodecs = &Ydb_Topic.SupportedCodecs{Codecs: []int32{int32(Ydb_Topic.Codec_CODEC_RAW)}}
	request := topicRequest(c, &ydbdecl.Topic{RetentionPeriod: "PT24H", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, read)

	result, err := backend.New().CompareModels(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	semantics := result.Changes[0].Value.ChangeSemantics
	c.Assert(semantics.Impact, qt.Equals, protocol.ChangeSemantics_DESTRUCTIVE)
	c.Assert(semantics.GetRemovesState(), qt.IsTrue)
	c.Assert(semantics.Reason, qt.Equals, "DROP CONSUMER removes a topic consumer and its position in the topic")
}

// plannedTopic is the captured change of the topic at directory/name of the
// fixture database: action "create" declares it with no setting, "drop"
// reads it as YDB 26.2.1.14 reports a new one, and "alter" reads it and
// declares a longer retention.
func plannedTopic(c *qt.C, directory, name, action string) *protocol.PlanningChange {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Schema: directory, Name: name}
	var before, after *protocol.Envelope
	var err error
	if action != "create" {
		before, err = schema.CaptureObject(owner, readTopic())
		c.Assert(err, qt.IsNil)
	}
	if action != "drop" {
		declared := ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "worker"}}}
		if action == "alter" {
			declared.RetentionPeriod = "P2D"
		}
		intent, err := topics.Intent(declared)
		c.Assert(err, qt.IsNil)
		after, err = schema.CaptureTopicIntent(owner, intent)
		c.Assert(err, qt.IsNil)
	}
	change, err := schema.CaptureTopicChange(directory+"/"+name, owner, before, after)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: testplanning.Subject(owner), Value: change}
}

// topicPlanRequest is the topic service's request for changes beside the
// host's steps common, on a target holding capabilities.
func topicPlanRequest(capabilities map[string]bool, common []*protocol.CommonPlanningStep, changes ...*protocol.PlanningChange) *protocol.PlanFeaturesRequest {
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: topicplan.ServiceID, DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities,
		Changes: changes, CommonSteps: common}
}

// A topic planned beside the host's steps at its path or a path above it
// takes the path once it is free: a creation follows the drop of the object
// at its path or at the directory above it, and a drop precedes the creation
// of an object at its path. A creation and a change act on the topic itself,
// which is what orders a streaming query or a transfer that reads it after
// them. Ported from the host's TestTopicPlan_PlacesEachStatement.
func TestTopicOrdersAgainstCommonStepsAtItsPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, action string
		common       []*protocol.CommonPlanningStep
		edges        []string
		effect       protocol.PlanningEffect_Action
	}{
		{name: "a creation follows the drop that frees its path", action: "create",
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "events", protocol.PlanningEffect_DROP)},
			edges:  []string{"drop table app/events -> topic/000000"}, effect: protocol.PlanningEffect_CREATE},
		{name: "a creation follows the drop of its directory", action: "create",
			common: []*protocol.CommonPlanningStep{commonAt("table", "", "app", protocol.PlanningEffect_DROP)},
			edges:  []string{"drop table /app -> topic/000000"}, effect: protocol.PlanningEffect_CREATE},
		{name: "a drop runs before a creation at its path", action: "drop",
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "events", protocol.PlanningEffect_CREATE)},
			edges:  []string{"topic/000000 -> create table app/events"}, effect: protocol.PlanningEffect_DROP},
		{name: "a creation with nothing at its path", action: "create", effect: protocol.PlanningEffect_CREATE},
		{name: "a change", action: "alter", effect: protocol.PlanningEffect_ALTER},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicPlanRequest(map[string]bool{"topics": true, "transactional_ddl": false}, test.common, plannedTopic(c, "app", "events", test.action))

			result, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			c.Assert(stepEdges(result), qt.DeepEquals, test.edges)
			effects := result.Contributions[0].Steps[0].Metadata.Effects
			c.Assert(effects[0].Subject.Kind, qt.Equals, schema.TopicProperties)
			c.Assert(effects[0].Action, qt.Equals, test.effect)
		})
	}
}

// One refused topic change refuses the batch: a target without topics, a
// change YDB makes in no line, and a creation at a path a host step creates a
// table at. The diagnostic names the refused change, and no other change of
// the batch is planned. Ported from the host's TestTopicPlan_RefusesTheWholeBatch.
func TestTopicPlanRefusesTheWholeBatch_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	fewer := plannedTopic(c, "app", "events", "alter")
	wide := readTopic()
	wide.PartitioningSettings.MinActivePartitions = 2
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Schema: "app", Name: "events"}
	before, err := schema.CaptureObject(owner, wide)
	c.Assert(err, qt.IsNil)
	_, after, err := schema.TopicOperands(fewer.Value.Value)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureTopicChange("app/events", owner, before, after)
	c.Assert(err, qt.IsNil)
	fewer.Value = change
	for _, test := range []struct {
		name   string
		topics bool
		second *protocol.PlanningChange
		common []*protocol.CommonPlanningStep
		change uint32
		want   string
	}{
		{name: "a target without topics", second: plannedTopic(c, "app", "events", "create"), change: 0,
			want: "Planning a topic requires the topics capability and nontransactional DDL"},
		{name: "fewer partitions", topics: true, second: fewer, change: 1, want: "topic app/events: it has 2 partitions and is declared with 1, .*"},
		{name: "a path a table takes", topics: true, second: plannedTopic(c, "app", "events", "create"), change: 1,
			common: []*protocol.CommonPlanningStep{commonAt("table", "app", "events", protocol.PlanningEffect_CREATE)},
			want:   "topic operation conflicts with a common occupant at its path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicPlanRequest(map[string]bool{"topics": test.topics, "transactional_ddl": false}, test.common, plannedTopic(c, "", "first", "create"), test.second)

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
