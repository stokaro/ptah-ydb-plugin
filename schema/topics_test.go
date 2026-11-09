package schema_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func topicOwner() *protocol.Reference {
	owner := tableOwner()
	owner.Kind = schema.TopicProperties
	owner.Name = "order.events"
	return owner
}

func nativeTopic() *native.DescribeTopicResult {
	return &native.DescribeTopicResult{
		Self:                 &Ydb_Scheme.Entry{Name: "events", Type: Ydb_Scheme.Entry_TOPIC, Owner: "alice", SizeBytes: 300, CreatedAt: &Ydb.VirtualTimestamp{PlanStep: 7}},
		PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 2}, RetentionPeriod: durationpb.New(12 * time.Hour),
		Attributes: map[string]string{"metadata": "retain"}, RetentionStorageMb: 100, MetricsLevel: proto.Uint32(2),
		Consumers:  []*native.Consumer{{Name: "z", Important: true, Attributes: map[string]string{"_service_type": "data-streams"}, ConsumerStats: &native.Consumer_ConsumerStats{}}, {Name: "a", ConsumerType: &native.Consumer_StreamingConsumerType{StreamingConsumerType: &native.StreamingConsumerType{}}}},
		Partitions: []*native.DescribeTopicResult_PartitionInfo{{PartitionId: 4, Active: true}}, TopicStats: &native.DescribeTopicResult_TopicStats{StoreSizeBytes: 500},
	}
}

func nativeFeed(name string) *native.ObservedChangefeed {
	return &native.ObservedChangefeed{Description: &native.ChangefeedDescription{
		Name: name, Mode: Ydb_Table.ChangefeedMode_MODE_NEW_IMAGE, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_ENABLED,
		InitialScanProgress: &native.ChangefeedDescription_InitialScanProgress{PartsTotal: 9, PartsCompleted: 9}, UserSids: true,
		Attributes: map[string]string{"__async_replication": `{"path":"/remote/replica","id":"7","supports_topic_autopartitioning":true}`},
	}, Topic: nativeTopic()}
}

func TestTopicObservationPreservesSettingsAndOmitsRuntimeState(t *testing.T) {
	c := testcheck.New(t)
	input := nativeTopic()
	result, err := schema.CaptureObject(topicOwner(), input)
	c.Assert(err, qt.IsNil)
	var decoded native.DescribeTopicResult
	c.Assert(protojson.Unmarshal(result.Payload, &decoded), qt.IsNil)
	c.Assert(decoded.Partitions, qt.HasLen, 0)
	c.Assert(decoded.TopicStats, qt.IsNil)
	c.Assert(decoded.GetSelf().GetName(), qt.Equals, "")
	c.Assert(decoded.GetSelf().GetOwner(), qt.Equals, "alice")
	c.Assert(decoded.GetSelf().GetSizeBytes(), qt.Equals, uint64(0))
	c.Assert(decoded.GetSelf().GetCreatedAt(), qt.IsNil)
	c.Assert(decoded.Attributes, qt.DeepEquals, input.Attributes)
	c.Assert(decoded.RetentionStorageMb, qt.Equals, int64(100))
	c.Assert(decoded.GetMetricsLevel(), qt.Equals, uint32(2))
	c.Assert(decoded.GetRetentionPeriod().AsDuration(), qt.Equals, 12*time.Hour)
	// The consumers keep the order YDB described them in, the order they
	// were added in.
	c.Assert(decoded.Consumers[0].Name, qt.Equals, "z")
	c.Assert(decoded.Consumers[0].ConsumerStats, qt.IsNil)
	c.Assert(decoded.Consumers[0].Attributes, qt.DeepEquals, input.Consumers[0].Attributes)
	c.Assert(decoded.Consumers[1].Name, qt.Equals, "a")
	c.Assert(decoded.Consumers[1].GetStreamingConsumerType(), qt.IsNotNil)
	c.Assert(input.Consumers[0].ConsumerStats, qt.IsNotNil)
	c.Assert(input.Partitions, qt.HasLen, 1)
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestChangefeedsRetainScanProvenanceAndOwnershipAttributes(t *testing.T) {
	c := testcheck.New(t)
	input := &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{nativeFeed("z"), nativeFeed("a")}}
	input.Changefeeds[1].Description.State = native.ChangefeedDescription_STATE_DISABLED
	result, err := schema.CaptureChangefeeds(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	var decoded native.ChangefeedObservations
	c.Assert(protojson.Unmarshal(result.Payload, &decoded), qt.IsNil)
	c.Assert(decoded.Changefeeds[0].Description.Name, qt.Equals, "a")
	c.Assert(decoded.Changefeeds[0].Description.State, qt.Equals, native.ChangefeedDescription_STATE_DISABLED)
	c.Assert(decoded.Changefeeds[0].InitialScan, qt.IsTrue)
	c.Assert(decoded.Changefeeds[0].Description.InitialScanProgress, qt.IsNil)
	c.Assert(decoded.Changefeeds[0].Description.UserSids, qt.IsTrue)
	c.Assert(decoded.Changefeeds[0].Description.Attributes, qt.DeepEquals, input.Changefeeds[1].Description.Attributes)
	c.Assert(decoded.Changefeeds[0].Topic.GetRetentionPeriod().AsDuration(), qt.Equals, 12*time.Hour)
	// A changefeed's topic keeps its consumers in the order its observation
	// lists them; a read lists them by name (see the inspection).
	c.Assert([]string{decoded.Changefeeds[0].Topic.Consumers[0].Name, decoded.Changefeeds[0].Topic.Consumers[1].Name}, qt.DeepEquals, []string{"z", "a"})
	c.Assert(input.Changefeeds[1].Description.InitialScanProgress.PartsTotal, qt.Equals, uint32(9))
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestStreamModelsRefuseIncompleteAndRuntimePayloads(t *testing.T) {
	c := testcheck.New(t)
	topic, err := schema.CaptureObject(topicOwner(), nativeTopic())
	c.Assert(err, qt.IsNil)
	feeds, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{})
	c.Assert(err, qt.IsNil)
	for _, test := range []struct {
		name, payload string
		model         *protocol.Envelope
	}{
		{"missing topic settings", `{}`, topic},
		{"runtime partitions", `{"partitioningSettings":{},"partitions":[{"partitionId":"4"}]}`, topic},
		{"runtime consumer stats", `{"partitioningSettings":{},"consumers":[{"name":"a","consumerStats":{}}]}`, topic},
		{"duplicate consumer", `{"partitioningSettings":{},"consumers":[{"name":"a"},{"name":"a"}]}`, topic},
		{"missing child topic", `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"}}]}`, feeds},
		{"path escape", `{"changefeeds":[{"description":{"name":"../f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`, feeds},
		{"unspecified mode", `{"changefeeds":[{"description":{"name":"f","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`, feeds},
		{"scan without provenance", `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_INITIAL_SCAN"},"topic":{"partitioningSettings":{}}}]}`, feeds},
		{"progress counters", `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED","initialScanProgress":{}},"topic":{"partitioningSettings":{}}}]}`, feeds},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value := proto.CloneOf(test.model)
			value.Payload = []byte(test.payload)
			result, err := schema.Normalize(value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

func TestUnknownConsumerArmsAndNestedMissingMessagesCannotDisappear(t *testing.T) {
	shared := nativeTopic()
	shared.Consumers[0].ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 10, protowire.BytesType), nil))
	missing := nativeTopic()
	missing.Consumers = append(missing.Consumers, nil)
	for _, test := range []struct {
		name  string
		value *native.DescribeTopicResult
	}{{"shared consumer", shared}, {"nil consumer", missing}} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := schema.CaptureObject(topicOwner(), test.value)
			c.Assert(value, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	c := testcheck.New(t)
	incomplete := &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{nativeFeed("a")}}
	incomplete.Changefeeds[0].Topic = nil
	value, err := schema.CaptureChangefeeds(tableOwner(), incomplete)
	c.Assert(value, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}

// initialScanRows are changefeeds as YDB reports them at each point of an
// initial scan, and whether each asked for one.
var initialScanRows = []struct {
	name     string
	state    native.ChangefeedDescription_State
	progress *native.ChangefeedDescription_InitialScanProgress
	want     bool
}{
	// 25.1.4.7 reports this right after CREATE ASYNC REPLICATION adds its
	// changefeed to the source table.
	{"scanning before the first progress", native.ChangefeedDescription_STATE_INITIAL_SCAN, nil, true},
	{"scanning with progress", native.ChangefeedDescription_STATE_INITIAL_SCAN, &native.ChangefeedDescription_InitialScanProgress{PartsTotal: 3, PartsCompleted: 1}, true},
	{"enabled after the scan", native.ChangefeedDescription_STATE_ENABLED, &native.ChangefeedDescription_InitialScanProgress{PartsTotal: 3, PartsCompleted: 3}, true},
	{"disabled after the scan", native.ChangefeedDescription_STATE_DISABLED, &native.ChangefeedDescription_InitialScanProgress{PartsTotal: 3, PartsCompleted: 3}, true},
	{"enabled without a scan", native.ChangefeedDescription_STATE_ENABLED, nil, false},
	{"disabled without a scan", native.ChangefeedDescription_STATE_DISABLED, nil, false},
}

// TestChangefeedInitialScan_HappyPath reads whether a changefeed asked for an
// initial scan from its scan state or its progress, through both the capture
// inspection makes and the projection a write confirms with, and keeps the
// progress counters out of either.
func TestChangefeedInitialScan_HappyPath(t *testing.T) {
	for _, test := range initialScanRows {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed := nativeFeed("f")
			observed.Description.State = test.state
			observed.Description.InitialScanProgress = test.progress

			captured, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{observed}})
			c.Assert(err, qt.IsNil)
			var decoded native.ChangefeedObservations
			c.Assert(protojson.Unmarshal(captured.Payload, &decoded), qt.IsNil)
			c.Assert(decoded.Changefeeds, qt.HasLen, 1)
			c.Assert(decoded.Changefeeds[0].InitialScan, qt.Equals, test.want)
			c.Assert(decoded.Changefeeds[0].Description.State, qt.Equals, test.state)
			c.Assert(decoded.Changefeeds[0].Description.InitialScanProgress, qt.IsNil)

			projected, err := schema.ProjectChangefeed(observed)
			c.Assert(err, qt.IsNil)
			c.Assert(projected.InitialScan, qt.Equals, test.want)
			c.Assert(projected.Description.InitialScanProgress, qt.IsNil)
		})
	}
}

// TestChangefeedInitialScan_ReadMatchesTheDeclaration compares the read of each
// changefeed with a declaration that states the scan it asked for: a read made
// before YDB reports any progress is no reason to drop and add the changefeed
// again.
func TestChangefeedInitialScan_ReadMatchesTheDeclaration(t *testing.T) {
	for _, test := range initialScanRows {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed := &native.ObservedChangefeed{Description: &native.ChangefeedDescription{Name: "f", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES,
				Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: test.state, InitialScanProgress: test.progress},
				Topic: &native.DescribeTopicResult{RetentionPeriod: durationpb.New(24 * time.Hour), PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1}}}
			projected, err := schema.ProjectChangefeed(observed)
			c.Assert(err, qt.IsNil)
			current, err := changefeeds.FromObserved(projected)
			c.Assert(err, qt.IsNil)
			desired := changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", InitialScan: test.want},
				Disabled: test.state == native.ChangefeedDescription_STATE_DISABLED}
			c.Assert(changefeeds.Recreated(desired, current), qt.IsFalse)
			c.Assert(changefeeds.Equal(desired, current), qt.IsTrue)
		})
	}
}

// A declared topic and a declared changefeed's topic keep their consumers in
// the order the declaration lists them, as the host's embedded engine wrote
// them into CREATE TOPIC and ALTER TOPIC ... ADD CONSUMER.
func TestDesiredConsumersKeepTheirOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	consumers := []*native.ConsumerIntent{{Name: "z"}, {Name: "a"}, {Name: "m"}}
	topic, err := schema.CaptureTopicIntent(topicOwner(), &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}, Consumers: consumers})
	c.Assert(err, qt.IsNil)
	feeds, err := schema.CaptureChangefeedIntent(tableOwner(), &native.ChangefeedSetIntent{Changefeeds: []*native.ChangefeedIntent{{Name: "f",
		Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON,
		Topic: &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}, Consumers: consumers}}}})
	c.Assert(err, qt.IsNil)

	declared, err := schema.DesiredTopic(topic)
	c.Assert(err, qt.IsNil)
	var set native.ChangefeedSetIntent
	c.Assert(protojson.Unmarshal(feeds.Payload, &set), qt.IsNil)

	names := func(consumers []*native.ConsumerIntent) []string {
		var listed []string
		for _, consumer := range consumers {
			listed = append(listed, consumer.GetName())
		}
		return listed
	}
	c.Assert(names(declared.Consumers), qt.DeepEquals, []string{"z", "a", "m"})
	c.Assert(names(set.Changefeeds[0].Topic.Consumers), qt.DeepEquals, []string{"z", "a", "m"})
}

// The observation predicted from a declared changefeed keeps its topic's
// consumers in the declaration's order, which the reverse of the change that
// declares them reads as what the change left: undoing a move to consumers z,
// a and m drops them in that order, as the host's embedded engine wrote the
// reverse.
func TestPredictedChangefeedKeepsTheDeclaredConsumerOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	feeds, err := schema.CaptureChangefeedIntent(tableOwner(), &native.ChangefeedSetIntent{Changefeeds: []*native.ChangefeedIntent{{Name: "f",
		Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON,
		Topic: &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}, Consumers: []*native.ConsumerIntent{{Name: "z"}, {Name: "a"}, {Name: "m"}}}}}})
	c.Assert(err, qt.IsNil)

	predicted, err := schema.ConvertNative(feeds, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)

	c.Assert(err, qt.IsNil)
	var set native.ChangefeedObservations
	c.Assert(protojson.Unmarshal(predicted.Payload, &set), qt.IsNil)
	c.Assert(set.Changefeeds, qt.HasLen, 1)
	var names []string
	for _, consumer := range set.Changefeeds[0].Topic.Consumers {
		names = append(names, consumer.GetName())
	}
	c.Assert(names, qt.DeepEquals, []string{"z", "a", "m"})
}
