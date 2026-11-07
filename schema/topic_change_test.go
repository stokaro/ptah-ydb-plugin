package schema_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/types/known/durationpb"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// topicPair is order.events, a topic of a partition and one consumer, as
// inspection captures it and the declaration converted from that read: one
// topic as YDB keeps it.
func topicPair(c *qt.C) (read, declared *protocol.Envelope) {
	c.Helper()
	read, err := schema.CaptureObject(topicOwner(), &native.DescribeTopicResult{PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1},
		RetentionPeriod: durationpb.New(18 * time.Hour), Consumers: []*native.Consumer{{Name: "audit"}}})
	c.Assert(err, qt.IsNil)
	declared, err = schema.ConvertNative(read, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	return read, declared
}

// A topic change takes exactly its two operands, at least one present, each
// a value of its own model, and two operands that describe one topic hold no
// change. Ported from the host's TestTopicChange_FailurePath.
func TestTopicChange_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	read, declared := topicPair(c)
	held, after := string(read.Payload), string(declared.Payload)
	for _, test := range []struct {
		name, payload, wantErr string
	}{
		{"null", `null`, `topic change requires exact before and after operands`},
		{"two absent operands", `{"before":null,"after":null}`, `topic change has no operands`},
		{"an omitted operand", `{"after":` + after + `}`, `topic change requires exact before and after operands`},
		{"a field the declaration does not hold", `{"before":null,"after":{"maxActivePartitions":4}}`, `invalid desired native YDB JSON`},
		{"a holder on the observation", `{"before":{"structName":"Events"},"after":null}`, `invalid YDB observation JSON`},
		{"an extra field", `{"before":null,"after":` + after + `,"extra":true}`, `topic change requires exact before and after operands`},
		{"two operands that describe one topic", `{"before":` + held + `,"after":` + after + `}`, `topic operands contain no change`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change, err := schema.NormalizeTopicChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE,
				Value: forgedChange(schema.TopicChangeModel(), topicOwner(), test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

// A topic operation refuses a payload with no change, an extra field or
// neither operand, and an owner with no name or a path in its name, which
// its statement would write as another topic. Ported from the host's
// TestTopicCodec_FailurePath.
func TestTopicOperation_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, declared := topicPair(c)
	creation := `{"before":null,"after":` + string(declared.Payload) + `}`
	for _, test := range []struct {
		name, owner, payload, wantErr string
	}{
		{"no change", "order.events", `{}`, `topic change requires exact before and after operands`},
		{"no name", "", creation, `invalid YDB observation owner`},
		{"a path in the name", "app/events", creation, `invalid YDB object path`},
		{"an extra field", "order.events", `{"before":null,"after":` + string(declared.Payload) + `,"extra":1}`, `topic change requires exact before and after operands`},
		{"neither operand", "order.events", `{"before":null,"after":null}`, `topic change has no operands`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := topicOwner()
			owner.Name = test.owner
			operation, err := schema.NormalizeTopicOperation(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION,
				Value: forgedChange(schema.TopicOperationModel(), owner, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(operation, qt.IsNil)
		})
	}
}
