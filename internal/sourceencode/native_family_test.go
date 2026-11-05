package sourceencode_test

import (
	"bytes"
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func familyValue(c *qt.C, value *protocol.Envelope, err error) *protocol.NormalizedValue {
	c.Helper()
	c.Assert(err, qt.IsNil)
	return &protocol.NormalizedValue{Id: "family", Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
		CanonicalFormat: schema.NativeDesiredModel(value.Kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}
}

func topicFamilyIntent(c *qt.C) *native.TopicIntent {
	c.Helper()
	intent, err := topics.Intent(ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 6, AutoPartitioningStrategy: "scale_up_and_down",
		AutoPartitioningUpUtilizationPercent: 70, AutoPartitioningDownUtilizationPercent: 30, AutoPartitioningStabilizationWindow: "PT2M",
		RetentionPeriod: "P1DT12H", PartitionWriteSpeedBytesPerSecond: 2097152, PartitionWriteBurstBytes: 4194304,
		SupportedCodecs: []string{"raw", "gzip"}, Consumers: []ydbdecl.Consumer{
			{Name: "billing", Important: true, SupportedCodecs: []string{"gzip"}},
			{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z", AvailabilityPeriod: "P2D"}}})
	c.Assert(err, qt.IsNil)
	return intent
}

func topicFamilyValue(c *qt.C, intent *native.TopicIntent) *protocol.NormalizedValue {
	c.Helper()
	value, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app/eu", Name: "a.b"}, intent)
	return familyValue(c, value, err)
}

// readTopicValue is a topic as a read converts it: with the attributes YDB
// keeps on it and its consumer, and with the settings YDB gives a new topic
// stated.
func readTopicValue(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	var intent native.TopicIntent
	c.Assert(protojson.Unmarshal([]byte(`{"attributes":{"_partitions_per_tablet":"1","_timestamp_type":"CreateTime"},`+
		`"consumers":[{"attributes":{"_service_type":"data-streams"},"important":true,"name":"audit","readFrom":"1970-01-01T00:00:00Z","streamingConsumerType":{}}],`+
		`"partitionWriteBurstBytes":"1048576","partitionWriteSpeedBytesPerSecond":"1048576","partitioningSettings":{"autoPartitioningSettings":{"partitionWriteSpeed":`+
		`{"downUtilizationPercent":20,"stabilizationWindow":"300s","upUtilizationPercent":80},"strategy":"AUTO_PARTITIONING_STRATEGY_DISABLED"},`+
		`"maxActivePartitions":"1","minActivePartitions":"2"},"retentionPeriod":"86400s"}`), &intent), qt.IsNil)
	return topicFamilyValue(c, &intent)
}

func poolFamilyValue(c *qt.C, spec workload.PoolSpec) *protocol.NormalizedValue {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: "batch.pool"}, schema.PoolSettings(spec))
	return familyValue(c, value, err)
}

func classifierFamilyValue(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: "reader.classifier"},
		schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: "batch.pool", MemberName: `reader "east"`, Rank: 0}))
	return familyValue(c, value, err)
}

func streamingFamilyValue(c *qt.C, spec streaming.Spec, allowStateReset bool) *protocol.NormalizedValue {
	c.Helper()
	value, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "jobs", Name: "copy"}, spec, allowStateReset)
	return familyValue(c, value, err)
}

type encoder func(protocol.SourceDocument_Format, *protocol.NormalizedValue) (sourceencode.Result, error)

type sourceDecoder func(context.Context, *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error)

func decodeParts(c *qt.C, format protocol.SourceDocument_Format, parts []*protocol.EncodedSourcePart, decode sourceDecoder) []*protocol.SourceModelValue {
	c.Helper()
	var fragments []*protocol.SourceFragment
	for _, part := range parts {
		fragments = append(fragments, part.Fragment)
	}
	decoded, err := decode(c.Context(), &protocol.SourceDocument{Id: "roundtrip", Format: format, Fragments: fragments})
	c.Assert(err, qt.IsNil)
	return decoded[0]
}

// Each native object is written so that its decoder gives back the same
// declaration, its canonical payload byte for byte, in YAML in one part and
// in Go in one part and one more for each topic consumer. Encoding twice
// writes the same parts.
func TestNativeFamiliesRoundTrip_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	emptyTopic, err := topics.Intent(ydbdecl.Topic{})
	c.Assert(err, qt.IsNil)
	for _, test := range []struct {
		name    string
		value   *protocol.NormalizedValue
		encode  encoder
		decode  sourceDecoder
		goParts int
	}{
		{"topic consumers", topicFamilyValue(c, topicFamilyIntent(c)), sourceencode.Topic, topicsource.Decode, 3},
		{"topic empty", topicFamilyValue(c, emptyTopic), sourceencode.Topic, topicsource.Decode, 1},
		{"pool present limits", poolFamilyValue(c, workload.PoolSpec{ConcurrentQueryLimit: new(int32(0)), QueueSize: new(int32(32)), DatabaseLoadCPUThreshold: new(12.5),
			QueryMemoryLimitPercentPerNode: new(25.5), QueryCPULimitPercentPerNode: new(50.0), TotalCPULimitPercentPerNode: new(75.25), ResourceWeight: new(0.0)}), sourceencode.ResourcePool, workloadsource.Decode, 1},
		{"pool unset limits", poolFamilyValue(c, workload.PoolSpec{}), sourceencode.ResourcePool, workloadsource.Decode, 1},
		{"classifier rank zero", classifierFamilyValue(c), sourceencode.ResourcePoolClassifier, workloadsource.Decode, 1},
		{"stopped streaming query", streamingFamilyValue(c, streaming.Spec{Text: "INSERT INTO `jobs/sink` SELECT * FROM `jobs/source`", Run: new(false), ResourcePool: "batch"}, true),
			sourceencode.StreamingQuery, streamingsource.Decode, 1},
		{"running streaming query", streamingFamilyValue(c, streaming.Spec{Text: "INSERT INTO `jobs/sink` SELECT * FROM `jobs/source`"}, false),
			sourceencode.StreamingQuery, streamingsource.Decode, 1},
	} {
		for _, format := range []struct {
			kind  protocol.SourceDocument_Format
			parts int
		}{
			{protocol.SourceDocument_YAML, 1}, {protocol.SourceDocument_GO_ANNOTATIONS, test.goParts},
		} {
			t.Run(test.name+"/"+format.kind.String(), func(t *testing.T) {
				c := testcheck.New(t)
				before := proto.CloneOf(test.value)
				result, err := test.encode(format.kind, test.value)
				c.Assert(err, qt.IsNil)
				c.Assert(result.Loss, qt.Equals, "")
				c.Assert(result.Parts, qt.HasLen, format.parts)
				c.Assert(test.value, qt.CmpEquals(protocmp.Transform()), before)
				decoded := decodeParts(c, format.kind, result.Parts, test.decode)
				c.Assert(decoded, qt.HasLen, 1)
				c.Assert(decoded[0].Value.Value.Owner, qt.CmpEquals(protocmp.Transform()), before.Value.Owner)
				c.Assert(string(decoded[0].Value.Value.Payload), qt.Equals, string(before.Value.Payload))
				again, err := test.encode(format.kind, test.value)
				c.Assert(err, qt.IsNil)
				c.Assert(again.Parts, qt.CmpEquals(protocmp.Transform()), result.Parts)
			})
		}
	}
}

// A topic a read converts is written as the declaration the topic comparison
// reads: what YDB keeps on the topic and its consumer is not written, the
// settings YDB gives a new topic are, and the decoded topic compares equal to
// the read one. A read names its database, which the source leaves to the
// host's connection.
func TestTopicFromARead_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			value := readTopicValue(c)
			value.Value.Owner.Database, value.NormalizedOwner.Database = "/local", "/local"
			value.Value.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
			result, err := sourceencode.Topic(format, value)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Loss, qt.Equals, "")
			decoded := decodeParts(c, format, result.Parts, topicsource.Decode)
			c.Assert(decoded, qt.HasLen, 1)
			c.Assert(decoded[0].Value.Value.Owner.Database, qt.Equals, "")
			got, err := schema.DesiredTopic(decoded[0].Value.Value)
			c.Assert(err, qt.IsNil)
			read, err := topics.FromIntent(got)
			c.Assert(err, qt.IsNil)
			c.Assert(read, qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 2, AutoPartitioningStrategy: "disabled", RetentionPeriod: "P1D",
				PartitionWriteSpeedBytesPerSecond: 1048576, PartitionWriteBurstBytes: 1048576, Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true}}})
		})
	}
}

// A setting no declaration states, an access requirement, a database other
// than the one the value was read in, and HCL are losses, and no part is
// written; the input is left as it was.
func TestNativeFamilies_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	withAccess := topicFamilyIntent(c)
	withAccess.Access = &native.AccessIntent{Owner: new("alice")}
	withStorage := topicFamilyIntent(c)
	withStorage.RetentionStorageMb = 100
	withMetrics := topicFamilyIntent(c)
	withMetrics.MetricsLevel = proto.Uint32(2)
	foreign := topicFamilyValue(c, topicFamilyIntent(c))
	foreign.Value.Owner.Database, foreign.NormalizedOwner.Database = "/identity", "/identity"
	for _, test := range []struct {
		name    string
		format  protocol.SourceDocument_Format
		value   *protocol.NormalizedValue
		encode  encoder
		message string
	}{
		{"topic access", protocol.SourceDocument_YAML, topicFamilyValue(c, withAccess), sourceencode.Topic, "the access requirement of topic app/eu/a.b has no source spelling"},
		{"topic retention storage", protocol.SourceDocument_GO_ANNOTATIONS, topicFamilyValue(c, withStorage), sourceencode.Topic,
			"topic app/eu/a.b: the declaration states retention_storage_mb, which no YQL statement sets"},
		{"topic metrics level", protocol.SourceDocument_YAML, topicFamilyValue(c, withMetrics), sourceencode.Topic,
			"topic app/eu/a.b: the declaration states metrics_level, which no YQL statement sets"},
		{"another database", protocol.SourceDocument_YAML, foreign, sourceencode.Topic, "topic app/eu/a.b names database /identity, which a source cannot name"},
		{"topic HCL", protocol.SourceDocument_HCL, topicFamilyValue(c, topicFamilyIntent(c)), sourceencode.Topic, "topic app/eu/a.b: HCL has no block that declares it"},
		{"pool HCL", protocol.SourceDocument_HCL, poolFamilyValue(c, workload.PoolSpec{}), sourceencode.ResourcePool, "resource pool batch.pool: HCL has no block that declares it"},
		{"classifier HCL", protocol.SourceDocument_HCL, classifierFamilyValue(c), sourceencode.ResourcePoolClassifier,
			"resource pool classifier reader.classifier: HCL has no block that declares it"},
		{"streaming query HCL", protocol.SourceDocument_HCL, streamingFamilyValue(c, streaming.Spec{Text: "SELECT 1"}, false), sourceencode.StreamingQuery,
			"streaming query jobs/copy: HCL has no block that declares it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			before := proto.CloneOf(test.value)
			result, err := test.encode(test.format, test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Loss, qt.Equals, test.message)
			c.Assert(result.Parts, qt.HasLen, 0)
			c.Assert(test.value, qt.CmpEquals(protocmp.Transform()), before)
		})
	}
}
