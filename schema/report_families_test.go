package schema_test

import (
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// TestReportValues_CoversFormerHostYDBStatsFamilies ports the YDB-only
// aggregates of the former host TestCollect_YDBFamilies. The host sends
// captured observations to ReportValues in one batch per model when it counts
// a read.
func TestReportValues_CoversFormerHostYDBStatsFamilies(t *testing.T) {
	c := testcheck.New(t)
	var values []*protocol.Envelope
	add := func(value *protocol.Envelope) { values = append(values, value) }
	capture := func(kind, name string, described proto.Message) {
		owner := objectOwner(kind)
		owner.Name = name
		value, err := schema.CaptureObject(owner, described)
		c.Assert(err, qt.IsNil)
		value.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
		add(value)
	}
	for i, consumers := range []int{2, 3} {
		topic := nativeTopic()
		topic.Consumers = topic.Consumers[:0]
		for n := range consumers {
			topic.Consumers = append(topic.Consumers, &native.Consumer{Name: "consumer_" + strconv.Itoa(n)})
		}
		capture(schema.TopicProperties, "topic_"+strconv.Itoa(i), topic)
	}
	for i, feedNames := range [][]string{{"updates", "keys"}, {"audit"}} {
		owner := tableOwner()
		owner.Name = "table_" + strconv.Itoa(i)
		feeds := &native.ChangefeedObservations{}
		for _, name := range feedNames {
			feeds.Changefeeds = append(feeds.Changefeeds, nativeFeed(name))
		}
		value, err := schema.CaptureChangefeeds(owner, feeds)
		c.Assert(err, qt.IsNil)
		add(value)
	}
	for i := range 4 {
		capture(schema.CoordinationProperties, "node_"+strconv.Itoa(i), &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}})
	}
	for i := range 5 {
		add(readPool(c, "pool_"+strconv.Itoa(i), workload.PoolSpec{}))
	}
	for i := range 6 {
		add(readClassifier(c, "classifier_"+strconv.Itoa(i), workload.ClassifierSpec{ResourcePool: "default", Rank: int64(i)}))
	}
	for i := range 2 {
		capture(schema.ReplicationProperties, "replication_"+strconv.Itoa(i), &native.AsyncReplicationSettings{
			Connection: &native.ReplicationConnection{ConnectionString: "grpc://primary:2136/?database=/prod"},
			Items:      []*native.ReplicationItem{{Source: "t", Target: "replica_" + strconv.Itoa(i)}}, State: "running"})
	}
	for i := range 3 {
		capture(schema.TransferProperties, "transfer_"+strconv.Itoa(i), &native.TransferSettings{
			Source: "events", Target: "orders", Lambda: "($m) -> { return []; }", State: "running"})
	}
	for i := range 9 {
		capture(schema.SecretProperties, "secret_"+strconv.Itoa(i), &native.SecretPresence{})
	}
	for i := range 10 {
		capture(schema.ExternalSourceProperties, "source_"+strconv.Itoa(i), &Ydb_Table.DescribeExternalDataSourceResult{
			SourceType: new("ObjectStorage"), Location: new(""), Properties: map[string]string{"AUTH_METHOD": "NONE"}})
	}
	for i, columns := range []int{2, 3, 1} {
		table := &native.DescribeExternalTableResult{SourceType: new("ObjectStorage"), DataSourcePath: new("/local/app/source_0"), Location: new("f/")}
		for column := range columns {
			table.Columns = append(table.Columns, &native.ColumnMeta{Name: "c" + strconv.Itoa(column), Type: primitive(Ydb.Type_INT64)})
		}
		capture(schema.ExternalTableProperties, "external_"+strconv.Itoa(i), table)
	}
	for i := range 11 {
		capture(schema.StreamingQueryProperties, "query_"+strconv.Itoa(i), &native.StreamingQuerySettings{Text: "SELECT 1;"})
	}

	description := backend.Description()
	negotiated := provider.NegotiatedDescription(description, provider.TwoWayFeatures())
	batches := make(map[string][]*protocol.Envelope)
	for _, value := range values {
		batches[value.GetKind()] = append(batches[value.GetKind()], value)
	}
	counts := make(map[string]uint64)
	for _, batch := range batches {
		request := &protocol.ReportValuesRequest{Engine: "ydb", TargetFeatures: negotiated.Engines[0].Features}
		for i, value := range batch {
			request.Values = append(request.Values, &protocol.ValueInput{Id: strconv.Itoa(i), Representation: protocol.ValueInput_OBSERVED, Value: value})
		}
		c.Assert(provider.ValidateReportValuesRequest(negotiated, request), qt.IsNil)
		reply, err := backend.New().ReportValues(c.Context(), request)
		c.Assert(err, qt.IsNil)
		c.Assert(provider.ValidateValueReports(negotiated, request, reply), qt.IsNil)
		for _, value := range reply.GetValues() {
			for _, count := range value.GetCounts() {
				counts[count.GetName()] += count.GetValue()
			}
		}
	}
	c.Assert(counts, qt.DeepEquals, map[string]uint64{
		"topics": 2, "topic_consumers": 5, "changefeeds": 3, "changefeed_consumers": 6,
		"coordination_nodes": 4, "resource_pools": 5, "resource_pool_classifiers": 6,
		"async_replications": 2, "transfers": 3, "secrets": 9,
		"external_data_sources": 10, "external_tables": 3, "external_columns": 6,
		"streaming_queries": 11,
	})
}
