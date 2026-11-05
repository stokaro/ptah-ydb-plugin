package backend_test

import (
	"bytes"
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

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

// placedContext is groupingContext from a host that reads Go placements.
func placedContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), append(provider.HostFeatures(groupingContext(c)), provider.GoPlacementsFeature))
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
	for _, test := range []struct {
		name  string
		ctx   func(*qt.C) context.Context
		after protocol.EncodedFragment_Placement
	}{
		{"a host that reads placements", placedContext, protocol.EncodedFragment_GO_AFTER_PRIVILEGES},
		{"a host that does not", groupingContext, protocol.EncodedFragment_PLACEMENT_UNSPECIFIED},
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

			encoded, err := backend.New().EncodeSource(test.ctx(c), request)

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
