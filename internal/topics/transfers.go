package topics

import (
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"google.golang.org/protobuf/proto"
)

// AdoptTransferConsumers returns intent with each consumer of the read topic
// that a transfer of the read reads through, and that intent does not name,
// added as the topic holds it: the consumer is the transfer's. It reports
// whether it added any; intent is not modified.
//
// YDB gives a transfer that names no consumer one of its own, an ordinary
// consumer with a generated name, and DROP TRANSFER drops it again. A
// declaration of the topic or the changefeed cannot name that consumer, so
// without the adoption a comparison plans `ALTER TOPIC ... DROP CONSUMER`
// under a running transfer, measured on 26.2.1.14 for a standalone topic and a
// changefeed. A consumer the transfer was given is adopted too, for as long as
// the read holds the transfer, as the host's embedded engine adopts it
// (`AdoptTransferConsumers` of its YDB lowering). The read marks such a
// consumer with [native.Consumer.Transfer].
func AdoptTransferConsumers(intent *native.TopicIntent, observed *native.DescribeTopicResult) (*native.TopicIntent, bool, error) {
	var adopted *native.TopicIntent
	for _, consumer := range observed.GetConsumers() {
		if consumer.GetTransfer() == "" || slices.ContainsFunc(intent.GetConsumers(), func(declared *native.ConsumerIntent) bool { return declared.GetName() == consumer.GetName() }) {
			continue
		}
		read, err := ObservedConsumer(consumer)
		if err != nil {
			return nil, false, err
		}
		kept, err := ConsumerIntent(read)
		if err != nil {
			return nil, false, err
		}
		if adopted == nil {
			adopted = proto.CloneOf(intent)
			if adopted == nil {
				adopted = &native.TopicIntent{}
			}
		}
		adopted.Consumers = append(adopted.Consumers, kept)
	}
	if adopted == nil {
		return intent, false, nil
	}
	return adopted, true, nil
}

// TransferConsumers names the consumers of observed the read marked as the
// ones a transfer reads the topic through, in the topic's order.
func TransferConsumers(observed *native.DescribeTopicResult) []string {
	var names []string
	for _, consumer := range observed.GetConsumers() {
		if consumer.GetTransfer() != "" {
			names = append(names, consumer.GetName())
		}
	}
	return names
}

// WithoutConsumers returns observed without the consumers names names, and
// without a transfer's mark on any consumer it keeps; observed is not
// modified. A read of the topic alone marks no consumer, and a consumer a
// transfer reads through is the transfer's, which the transfer's drop may take
// before a statement on the topic runs, so an operation on the topic compares
// what it reads with what the plan read without them.
func WithoutConsumers(observed *native.DescribeTopicResult, names []string) *native.DescribeTopicResult {
	cleared := proto.CloneOf(observed)
	cleared.Consumers = slices.DeleteFunc(cleared.Consumers, func(consumer *native.Consumer) bool { return slices.Contains(names, consumer.GetName()) })
	for _, consumer := range cleared.Consumers {
		consumer.Transfer = ""
	}
	return cleared
}
