package topics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Topics as YDB 26.2.1.14 describes them, read through the provider's
// inspection (payloads without the access metadata): one created by a plain
// CREATE TOPIC, and one with consumers and settings. 25.1.4.7 differs only
// in attributes and in leaving the streaming consumer type out.
const (
	plainTopic = `{"attributes":{"_max_partition_message_groups_seqno_stored":"6000000","_message_group_seqno_retention_period_ms":"1382400000","_partitions_per_tablet":"1","_timestamp_type":"CreateTime"},` +
		`"partitionWriteBurstBytes":"1048576","partitionWriteSpeedBytesPerSecond":"1048576",` +
		`"partitioningSettings":{"autoPartitioningSettings":{"partitionWriteSpeed":{"downUtilizationPercent":20,"stabilizationWindow":"300s","upUtilizationPercent":80},"strategy":"AUTO_PARTITIONING_STRATEGY_DISABLED"},"maxActivePartitions":"1","minActivePartitions":"1"},` +
		`"retentionPeriod":"86400s"}`
	fullTopic = `{"attributes":{"_max_partition_message_groups_seqno_stored":"6000000","_message_group_seqno_retention_period_ms":"1382400000","_partitions_per_tablet":"1","_timestamp_type":"CreateTime"},` +
		`"consumers":[{"attributes":{"_service_type":"data-streams"},"important":true,"name":"c1","readFrom":"2026-01-01T00:00:00Z","streamingConsumerType":{}},` +
		`{"attributes":{"_service_type":"data-streams"},"name":"c2","readFrom":"1970-01-01T00:00:00Z","streamingConsumerType":{}}],` +
		`"partitionWriteBurstBytes":"2097152","partitionWriteSpeedBytesPerSecond":"2097152",` +
		`"partitioningSettings":{"autoPartitioningSettings":{"partitionWriteSpeed":{"downUtilizationPercent":20,"stabilizationWindow":"300s","upUtilizationPercent":80},"strategy":"AUTO_PARTITIONING_STRATEGY_DISABLED"},"maxActivePartitions":"1","minActivePartitions":"2"},` +
		`"retentionPeriod":"129600s","supportedCodecs":{"codecs":[1,2]}}`
)

func observed(c *qt.C, text string) *native.DescribeTopicResult {
	c.Helper()
	var result native.DescribeTopicResult
	c.Assert(protojson.Unmarshal([]byte(text), &result), qt.IsNil)
	return &result
}

// A description reads as the host's reader reads it: the maximum and the
// thresholds left out while auto-partitioning is off, intervals as ISO 8601,
// the epoch read_from as none, and no attribute.
func TestFromObserved_HappyPath(t *testing.T) {
	c := qt.New(t)
	plain, err := topics.FromObserved(observed(c, plainTopic))
	c.Assert(err, qt.IsNil)
	c.Assert(plain, qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 1, AutoPartitioningStrategy: "disabled", RetentionPeriod: "P1D",
		PartitionWriteSpeedBytesPerSecond: 1048576, PartitionWriteBurstBytes: 1048576})
	full, err := topics.FromObserved(observed(c, fullTopic))
	c.Assert(err, qt.IsNil)
	c.Assert(full, qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 2, AutoPartitioningStrategy: "disabled", RetentionPeriod: "P1DT12H",
		PartitionWriteSpeedBytesPerSecond: 2097152, PartitionWriteBurstBytes: 2097152, SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "c1", Important: true, ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "c2"}}})
}

// A topic holding a setting no declaration states is refused with the
// host's words, as the host's reader refuses it.
func TestFromObserved_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*native.DescribeTopicResult)
		want   string
	}{
		{"a storage limit", func(d *native.DescribeTopicResult) { d.RetentionStorageMb = 10 }, `it holds retention_storage_mb, which Ptah does not model and no YQL statement sets; read a schema that does not hold the topic`},
		{"a metering mode", func(d *native.DescribeTopicResult) {
			d.MeteringMode = Ydb_Topic.MeteringMode_METERING_MODE_RESERVED_CAPACITY
		}, `it holds metering_mode, .*`},
		{"a metrics level", func(d *native.DescribeTopicResult) { d.MetricsLevel = new(uint32(2)) }, `it holds metrics_level, .*`},
		{"an unknown codec", func(d *native.DescribeTopicResult) {
			d.SupportedCodecs = &Ydb_Topic.SupportedCodecs{Codecs: []int32{7}}
		}, `its codec list names codec 7, which Ptah does not declare`},
		{"a fraction of a second", func(d *native.DescribeTopicResult) { d.RetentionPeriod = &durationpb.Duration{Seconds: 1, Nanos: 5} }, `its retention period is 1.000000005s, .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			description := observed(c, plainTopic)
			test.modify(description)
			spec, err := topics.FromObserved(description)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{})
		})
	}
}

// A declaration written as the provider's intent reads back as itself, and a
// read topic kept as it is reads as the same topic.
func TestIntent_HappyPath(t *testing.T) {
	c := qt.New(t)
	declared := ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 6, AutoPartitioningStrategy: "scale_up",
		AutoPartitioningUpUtilizationPercent: 70, AutoPartitioningStabilizationWindow: "PT2M", RetentionPeriod: "P1DT12H",
		PartitionWriteSpeedBytesPerSecond: 2097152, SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true}, {Name: "audit", ReadFrom: "2026-01-01T00:00:00Z", AvailabilityPeriod: "P2D"}}}
	intent, err := topics.Intent(declared)
	c.Assert(err, qt.IsNil)
	read, err := topics.FromIntent(intent)
	c.Assert(err, qt.IsNil)
	c.Assert(read, qt.DeepEquals, declared)
	kept, err := topics.Keep(observed(c, fullTopic))
	c.Assert(err, qt.IsNil)
	keptSpec, err := topics.FromIntent(kept)
	c.Assert(err, qt.IsNil)
	full, err := topics.FromObserved(observed(c, fullTopic))
	c.Assert(err, qt.IsNil)
	c.Assert(topics.Equal(keptSpec, full), qt.IsTrue)
	c.Assert(kept.Attributes, qt.HasLen, 0)
	c.Assert(kept.Access, qt.IsNil)
}

// An intent states only what a declaration can: one converted from a read
// topic keeps YDB's attributes, which are left out, and a setting no
// statement sets is refused.
func TestFromIntent_FailurePath(t *testing.T) {
	c := qt.New(t)
	spec, err := topics.FromIntent(&native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}, RetentionStorageMb: 5})
	c.Assert(err, qt.ErrorMatches, `the declaration states retention_storage_mb, which no YQL statement sets`)
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{})
	spec, err = topics.FromIntent(nil)
	c.Assert(err, qt.ErrorMatches, `missing YDB topic declaration`)
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{})
}

// A declaration that names a setting at the value YDB gives a new topic, and
// one that leaves it out, are the topic a plain CREATE TOPIC makes.
func TestEqual_HappyPath(t *testing.T) {
	c := qt.New(t)
	plain, err := topics.FromObserved(observed(c, plainTopic))
	c.Assert(err, qt.IsNil)
	for _, declared := range []ydbdecl.Topic{{}, {MinActivePartitions: 1, RetentionPeriod: "PT24H", PartitionWriteSpeedBytesPerSecond: 1048576}, {AutoPartitioningStrategy: "disabled"}} {
		c.Assert(topics.Equal(declared, plain), qt.IsTrue, qt.Commentf("%+v", declared))
	}
	c.Assert(topics.Equal(ydbdecl.Topic{RetentionPeriod: "PT2H"}, plain), qt.IsFalse)
	c.Assert(topics.Equal(ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "c"}}}, plain), qt.IsFalse)
}
