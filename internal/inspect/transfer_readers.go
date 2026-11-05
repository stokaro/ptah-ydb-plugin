package inspect

import (
	"path"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
)

// markTransferReaders marks each consumer of a topic or a changefeed the read
// holds that a transfer the read holds reads through
// ([native.Consumer.Transfer]), with the transfer's path below the database.
// The comparisons keep a marked consumer the declaration of its topic or its
// changefeed leaves out, since it is the transfer's; see
// [topics.AdoptTransferConsumers]. A transfer of another database, one whose
// description names no consumer yet, and a topic or a changefeed outside the
// read mark nothing.
func markTransferReaders(snapshot *protocol.Schema) error {
	readers := map[string]map[string]string{}
	for _, value := range snapshot.GetNativeObjects() {
		if value.GetKind() != schema.TransferProperties {
			continue
		}
		spec, _, err := schema.ObservedTransfer(value)
		if err != nil {
			return err
		}
		if !replication.LocalSource(spec) || spec.Consumer == "" || strings.HasPrefix(spec.Source, "/") {
			continue
		}
		source := path.Clean(spec.Source)
		if readers[source] == nil {
			readers[source] = map[string]string{}
		}
		readers[source][spec.Consumer] = replication.TablePath(value.GetOwner().GetSchema(), value.GetOwner().GetName())
	}
	if len(readers) == 0 {
		return nil
	}
	for i, value := range snapshot.GetNativeObjects() {
		if value.GetKind() != schema.TopicProperties {
			continue
		}
		consumers := readers[replication.TablePath(value.GetOwner().GetSchema(), value.GetOwner().GetName())]
		if consumers == nil {
			continue
		}
		topic, err := schema.ObservedTopic(value)
		if err != nil {
			return err
		}
		if !markConsumers(topic, consumers) {
			continue
		}
		marked, err := schema.CaptureObject(value.GetOwner(), topic)
		if err != nil {
			return err
		}
		marked.Context = value.GetContext()
		snapshot.NativeObjects[i] = marked
	}
	for _, table := range snapshot.GetTables() {
		for i, facet := range table.GetFacets() {
			if facet.GetKind() != schema.ChangefeedProperties {
				continue
			}
			var feeds native.ChangefeedObservations
			if err := protojson.Unmarshal(facet.GetPayload(), &feeds); err != nil {
				return err
			}
			marked := false
			for _, feed := range feeds.GetChangefeeds() {
				consumers := readers[path.Join(replication.TablePath(table.GetIdentity().GetSchema(), table.GetIdentity().GetName()), feed.GetDescription().GetName())]
				marked = markConsumers(feed.GetTopic(), consumers) || marked
			}
			if !marked {
				continue
			}
			captured, err := schema.CaptureChangefeeds(table.GetIdentity(), &feeds)
			if err != nil {
				return err
			}
			table.Facets[i] = captured
		}
	}
	return nil
}

// markConsumers marks each consumer of topic that consumers names with the
// transfer it names, and reports whether it marked any.
func markConsumers(topic *native.DescribeTopicResult, consumers map[string]string) bool {
	marked := false
	for _, consumer := range topic.GetConsumers() {
		if transfer, found := consumers[consumer.GetName()]; found {
			consumer.Transfer, marked = transfer, true
		}
	}
	return marked
}
