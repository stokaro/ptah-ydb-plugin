package schema_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
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
	c := qt.New(t)
	input := nativeTopic()
	result, err := schema.CaptureTopic(topicOwner(), input)
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
	c.Assert(decoded.Consumers[0].Name, qt.Equals, "a")
	c.Assert(decoded.Consumers[0].GetStreamingConsumerType(), qt.IsNotNil)
	c.Assert(decoded.Consumers[1].ConsumerStats, qt.IsNil)
	c.Assert(decoded.Consumers[1].Attributes, qt.DeepEquals, input.Consumers[0].Attributes)
	c.Assert(input.Consumers[0].ConsumerStats, qt.IsNotNil)
	c.Assert(input.Partitions, qt.HasLen, 1)
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestChangefeedsRetainScanProvenanceAndOwnershipAttributes(t *testing.T) {
	c := qt.New(t)
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
	c.Assert(input.Changefeeds[1].Description.InitialScanProgress.PartsTotal, qt.Equals, uint32(9))
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestStreamModelsRefuseIncompleteAndRuntimePayloads(t *testing.T) {
	c := qt.New(t)
	topic, err := schema.CaptureTopic(topicOwner(), nativeTopic())
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
			c := qt.New(t)
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
			c := qt.New(t)
			value, err := schema.CaptureTopic(topicOwner(), test.value)
			c.Assert(value, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	c := qt.New(t)
	incomplete := &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{nativeFeed("a")}}
	incomplete.Changefeeds[0].Topic = nil
	value, err := schema.CaptureChangefeeds(tableOwner(), incomplete)
	c.Assert(value, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}
