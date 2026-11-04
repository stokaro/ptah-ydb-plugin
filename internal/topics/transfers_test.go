package topics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// readers is a read topic with consumer generated, which transfer app/ingest
// reads through, and consumer audit, which no transfer reads.
func readers() *native.DescribeTopicResult {
	return &native.DescribeTopicResult{
		PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1},
		Consumers: []*native.Consumer{
			{Name: "generated", Important: true, Transfer: "app/ingest"},
			{Name: "audit"},
		},
	}
}

// consumerNames names the consumers of intent in order.
func consumerNames(intent *native.TopicIntent) []string {
	var names []string
	for _, consumer := range intent.GetConsumers() {
		names = append(names, consumer.GetName())
	}
	return names
}

// TestAdoptTransferConsumers_HappyPath gives a declaration the consumer a
// transfer reads the topic through, as the topic holds it, where the
// declaration leaves it out; a consumer the declaration names keeps its
// declared settings, and one no transfer reads is not adopted.
func TestAdoptTransferConsumers_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		intent    *native.TopicIntent
		adopted   bool
		want      []string
		important bool
	}{
		{name: "no declared consumer", intent: &native.TopicIntent{}, adopted: true, want: []string{"generated"}, important: true},
		{name: "no declared settings at all", adopted: true, want: []string{"generated"}, important: true},
		{name: "another declared consumer", intent: &native.TopicIntent{Consumers: []*native.ConsumerIntent{{Name: "audit"}}}, adopted: true,
			want: []string{"audit", "generated"}, important: true},
		{name: "the consumer declared with other settings", intent: &native.TopicIntent{Consumers: []*native.ConsumerIntent{{Name: "generated"}}},
			want: []string{"generated"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			original := proto.CloneOf(test.intent)

			intent, adopted, err := topics.AdoptTransferConsumers(test.intent, readers())

			c.Assert(err, qt.IsNil)
			c.Assert(adopted, qt.Equals, test.adopted)
			c.Assert(consumerNames(intent), qt.DeepEquals, test.want)
			c.Assert(intent.GetConsumers()[len(intent.GetConsumers())-1].GetImportant(), qt.Equals, test.important)
			c.Assert(proto.Equal(test.intent, original), qt.IsTrue)
		})
	}
}

// TestAdoptTransferConsumers_FailurePath refuses to adopt a consumer the
// provider cannot declare, one whose read_from is not a whole second.
func TestAdoptTransferConsumers_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	read := readers()
	read.Consumers[0].ReadFrom = &timestamppb.Timestamp{Seconds: 1767225600, Nanos: 5}

	intent, adopted, err := topics.AdoptTransferConsumers(&native.TopicIntent{}, read)

	c.Assert(err, qt.ErrorMatches, `its read_from .* is not a whole second`)
	c.Assert(adopted, qt.IsFalse)
	c.Assert(intent, qt.IsNil)
}

// TestWithoutConsumers names the consumers a read marked as a transfer's, and
// leaves them out of the topic it returns, with no mark on the rest, without
// changing the read.
func TestWithoutConsumers(t *testing.T) {
	c := testcheck.New(t)
	marked := readers()
	marked.Consumers[1].Transfer = ""

	names := topics.TransferConsumers(marked)
	cleared := topics.WithoutConsumers(marked, names)

	c.Assert(names, qt.DeepEquals, []string{"generated"})
	c.Assert(cleared.GetConsumers(), qt.HasLen, 1)
	c.Assert(cleared.GetConsumers()[0].GetName(), qt.Equals, "audit")
	c.Assert(marked.GetConsumers(), qt.HasLen, 2)
	c.Assert(marked.GetConsumers()[0].GetTransfer(), qt.Equals, "app/ingest")
}
