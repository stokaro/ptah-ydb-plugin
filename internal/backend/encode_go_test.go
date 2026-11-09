package backend_test

import (
	"bytes"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's TestRender_StreamingQuery_RoundTrip: an exported Go
// annotation keeps a query's text, pool, run setting, and permission to reset
// state through the same EncodeSource and DecodeSource calls the host makes.
func TestEncodeSource_GoStreamingQueryRoundTrip_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "jobs", Name: "copy"}
	text := "$tag = \"it's a stream\";\nINSERT INTO output SELECT * FROM input;"
	query, err := schema.CaptureStreamingQueryIntent(owner, streaming.Spec{Text: text, Run: new(false), ResourcePool: "batch"}, true)
	c.Assert(err, qt.IsNil)
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
		Value: &protocol.NormalizedValue{Id: "query", Value: query, NormalizedOwner: proto.CloneOf(owner),
			CanonicalFormat: schema.NativeDesiredModel(schema.StreamingQueryProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(query.Payload)}})
	request.TargetFeatures = []string{provider.PlanningObjectContextFeature}
	ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature,
		provider.EncodeGroupsFeature, provider.PlanningObjectContextFeature})

	encoded, err := backend.New().EncodeSource(ctx, request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(goLines(encoded), qt.DeepEquals, []goLine{{Directive: "ptah:schema:streamingquery", Attributes: []string{
		"name=copy", "schema=jobs", "text=" + text, "run=false", "resource_pool=batch", "allow_state_reset=true",
	}}})
	readBack := sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)
	readBack.TargetFeatures = request.TargetFeatures
	c.Assert(decodedProjections(c, ctx, readBack), qt.DeepEquals,
		map[string]string{schema.StreamingQueryProperties + " jobs/copy": string(query.Payload)})
}

// workloadValue is the desired value of a resource pool or a classifier
// with settings.
func workloadValue(c *qt.C, id, kind, name string, settings proto.Message) *protocol.EncodeSourceValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Name: name}
	value, err := schema.CaptureWorkloadIntent(owner, settings)
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: id, Value: value,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// settledTopic is the desired value of topic app/events with settings whose
// names sort in another order than its directive writes them, and one
// consumer.
func settledTopic(c *qt.C) *protocol.EncodeSourceValue {
	c.Helper()
	intent, err := topics.Intent(ydbdecl.Topic{MinActivePartitions: 2, AutoPartitioningStrategy: "disabled", RetentionPeriod: "PT18H",
		PartitionWriteBurstBytes: 1048576, Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true, ReadFrom: "2026-01-01T00:00:00Z"}}})
	c.Assert(err, qt.IsNil)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}
	value, err := schema.CaptureTopicIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: "topic", Value: value,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(schema.TopicProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// goLine is a written directive as the export's comment shows it: its name,
// where it stands, and its attributes in order.
type goLine struct {
	Directive  string
	Placement  protocol.EncodedFragment_Placement
	Attributes []string
}

func goLines(encoded *protocol.EncodedSource) []goLine {
	var lines []goLine
	for _, part := range provider.EncodedSourceParts(encoded) {
		line := goLine{Directive: part.GetFragment().GetGoDirective().GetDirective(), Placement: part.GetPlacement()}
		for _, entry := range part.GetFragment().GetValue().GetMapping().GetEntries() {
			line.Attributes = append(line.Attributes, entry.GetKey()+"="+entry.GetValue().GetText())
		}
		lines = append(lines, line)
	}
	return lines
}

// A Go export writes the directives of the document as the host's embedded
// engine wrote them, measured with the host at 74de0b65b on YDB 25.1.4.7 and
// 26.2.1.14: topics and their consumers first, then resource pools, their
// classifiers and the coordination nodes, each with its attributes in the
// order the engine wrote them, whatever order the request gives the values
// in. A host that reads placements writes the last three after the default
// privileges, below the grants; one that does not writes every directive
// after the views.
func TestEncodeSource_GoDirectivesOfTheDocument_HappyPath(t *testing.T) {
	poolWritten := []string{"name=batch", "concurrent_query_limit=5", "queue_size=10", "resource_weight=3"}
	classifierWritten := []string{"name=batching", "resource_pool=batch", "member_name=root", "rank=1000"}
	topicWritten := []string{"name=events", "schema=app", "min_active_partitions=2", "auto_partitioning_strategy=disabled",
		"retention_period=PT18H", "partition_write_burst_bytes=1048576"}
	consumerWritten := []string{"name=audit", "topic=events", "schema=app", "important=true", "read_from=2026-01-01T00:00:00Z"}
	nodeWritten := []string{"name=queue", "schema=app"}
	grouping := []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.EncodeGroupsFeature}
	for _, test := range []struct {
		name     string
		features []string
		after    protocol.EncodedFragment_Placement
	}{
		{"a host that reads placements", append(slices.Clone(grouping), provider.GoPlacementsFeature), protocol.EncodedFragment_GO_AFTER_PRIVILEGES},
		{"a host that does not", grouping, protocol.EncodedFragment_PLACEMENT_UNSPECIFIED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS,
				node(c, "node", "app", "queue", schema.CoordinationIntent{}),
				workloadValue(c, "classifier", schema.ResourcePoolClassifierProperties, "batching",
					schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: "batch", MemberName: "root", Rank: 1000})),
				workloadValue(c, "pool", schema.ResourcePoolProperties, "batch",
					schema.PoolSettings(workload.PoolSpec{ConcurrentQueryLimit: new(int32(5)), QueueSize: new(int32(10)), ResourceWeight: new(float64(3))})),
				settledTopic(c))

			encoded, err := backend.New().EncodeSource(provider.ContextWithHostFeatures(c.Context(), test.features), request)

			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(goLines(encoded), qt.DeepEquals, []goLine{
				{Directive: "ptah:schema:topic", Attributes: topicWritten},
				{Directive: "ptah:schema:topic:consumer", Attributes: consumerWritten},
				{Directive: "ptah:schema:resourcepool", Placement: test.after, Attributes: poolWritten},
				{Directive: "ptah:schema:resourcepool:classifier", Placement: test.after, Attributes: classifierWritten},
				{Directive: "ptah:schema:coordinationnode", Placement: test.after, Attributes: nodeWritten},
			})
		})
	}
}

// A Go export writes each setting a node declares, a period in the ISO 8601
// text its directive reads, a fraction of a second included, and the settings
// in the order of the directive: the self-check period, the grace period, the
// read, attach and counters modes. Ported from the host's ydbcoordination
// TestAttributes_WritesTheSettingsTheSpecSets.
func TestEncodeSource_GoWritesEveryNodeSettingInOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, node(c, "node", "app", "queue", schema.CoordinationIntent{
		SelfCheckPeriodMillis: new(uint32(1500)), SessionGracePeriodMillis: new(uint32(2500)), ReadConsistencyMode: new("strict"),
		AttachConsistencyMode: new("relaxed"), RateLimiterCountersMode: new("detailed"),
	}))

	encoded, err := backend.New().EncodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.EncodeGroupsFeature}), request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(goLines(encoded), qt.DeepEquals, []goLine{{Directive: "ptah:schema:coordinationnode", Attributes: []string{
		"name=queue", "schema=app", "self_check_period=PT1.5S", "session_grace_period=PT2.5S",
		"read_consistency_mode=strict", "attach_consistency_mode=relaxed", "rate_limiter_counters_mode=detailed",
	}}})
}
