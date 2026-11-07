package schema_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// countText projects counts to name=value text, which a failed assertion
// prints without formatting protobuf messages.
func countText(counts []*protocol.MetricCount) []string {
	result := make([]string, 0, len(counts))
	for _, count := range counts {
		result = append(result, fmt.Sprintf("%s=%d", count.GetName(), count.GetValue()))
	}
	return result
}

// reportText projects reports to kind/representation: name "help" text.
func reportText(reports []*protocol.ModelReport) []string {
	var result []string
	for _, report := range reports {
		for _, metric := range report.GetMetrics() {
			result = append(result, fmt.Sprintf("%s/%s: %s %q", report.GetKind()[len(schema.ProviderID)+1:], report.GetRepresentation(), metric.GetName(), metric.GetHelp()))
		}
	}
	return result
}

// The metric names and help texts are the built-in YDB engine's, so a scrape
// through the provider has the lines a scrape through that engine has; the
// desired and observed models report the same metrics.
func TestReports_NameTheBuiltInEnginesMetrics(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(reportText(schema.Reports()), qt.DeepEquals, []string{
		`topic/DESIRED: topics "Standalone topics"`, `topic/DESIRED: topic_consumers "Consumers of standalone topics"`,
		`topic/OBSERVED: topics "Standalone topics"`, `topic/OBSERVED: topic_consumers "Consumers of standalone topics"`,
		`table-changefeeds/DESIRED: changefeeds "Table changefeeds"`, `table-changefeeds/DESIRED: changefeed_consumers "Consumers of table changefeeds"`,
		`table-changefeeds/OBSERVED: changefeeds "Table changefeeds"`, `table-changefeeds/OBSERVED: changefeed_consumers "Consumers of table changefeeds"`,
		`coordination-node/DESIRED: coordination_nodes "Standalone coordination nodes"`, `coordination-node/OBSERVED: coordination_nodes "Standalone coordination nodes"`,
		`streaming-query/DESIRED: streaming_queries "Standalone streaming queries"`, `streaming-query/OBSERVED: streaming_queries "Standalone streaming queries"`,
		`external-data-source/DESIRED: external_data_sources "External data sources"`, `external-data-source/OBSERVED: external_data_sources "External data sources"`,
		`external-table/DESIRED: external_tables "External tables"`, `external-table/DESIRED: external_columns "Columns across external tables"`,
		`external-table/OBSERVED: external_tables "External tables"`, `external-table/OBSERVED: external_columns "Columns across external tables"`,
		`async-replication/DESIRED: async_replications "Async replications"`, `async-replication/OBSERVED: async_replications "Async replications"`,
		`transfer/DESIRED: transfers "Transfers"`, `transfer/OBSERVED: transfers "Transfers"`,
		`secret/DESIRED: secrets "Secret objects, without their values"`, `secret/OBSERVED: secrets "Secret objects, without their values"`,
		`resource-pool/DESIRED: resource_pools "Resource pools"`, `resource-pool/OBSERVED: resource_pools "Resource pools"`,
		`resource-pool-classifier/DESIRED: resource_pool_classifiers "Resource pool classifiers"`,
		`resource-pool-classifier/OBSERVED: resource_pool_classifiers "Resource pool classifiers"`,
		`row-table-properties/DESIRED: ydb_ttl_tables "Tables with a captured YDB TTL"`,
		`row-table-properties/DESIRED: ydb_column_families "YDB column families a table states, without a default family that states nothing"`,
		`row-table-properties/DESIRED: ydb_table_partitioning "YDB row tables stating partitioning, read replicas or a key bloom filter"`,
		`row-table-properties/OBSERVED: ydb_ttl_tables "Tables with a captured YDB TTL"`,
		`row-table-properties/OBSERVED: ydb_column_families "YDB column families a table states, without a default family that states nothing"`,
		`row-table-properties/OBSERVED: ydb_table_partitioning "YDB row tables stating partitioning, read replicas or a key bloom filter"`,
		`index-properties/DESIRED: ydb_index_partitioning "YDB global indexes stating partitioning or read replicas"`,
		`index-properties/OBSERVED: ydb_index_partitioning "YDB global indexes stating partitioning or read replicas"`,
		`column-storage-properties/DESIRED: ydb_ttl_tables "Tables with a captured YDB TTL"`,
		`column-storage-properties/DESIRED: ydb_column_tables "YDB tables stored by column"`,
		`column-storage-properties/DESIRED: ydb_tiered_ttl_tables "YDB column tables whose TTL moves rows to an external data source"`,
		`column-storage-properties/DESIRED: indexes "Indexes"`,
		`column-storage-properties/OBSERVED: ydb_ttl_tables "Tables with a captured YDB TTL"`,
		`column-storage-properties/OBSERVED: ydb_column_tables "YDB tables stored by column"`,
		`column-storage-properties/OBSERVED: ydb_tiered_ttl_tables "YDB column tables whose TTL moves rows to an external data source"`,
		`column-storage-properties/OBSERVED: indexes "Indexes"`,
		`table-indexes/DESIRED: ydb_index_partitioning "YDB global indexes stating partitioning or read replicas"`,
		`table-indexes/DESIRED: ydb_vector_indexes "YDB vector indexes with captured settings"`,
		`table-indexes/DESIRED: indexes "Indexes"`,
		`table-indexes/OBSERVED: ydb_index_partitioning "YDB global indexes stating partitioning or read replicas"`,
		`table-indexes/OBSERVED: ydb_vector_indexes "YDB vector indexes with captured settings"`,
		`table-indexes/OBSERVED: indexes "Indexes"`,
	})
}

func TestCountValue_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	topic, err := schema.CaptureObject(topicOwner(), nativeTopic())
	c.Assert(err, qt.IsNil)
	feeds, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{nativeFeed("a"), nativeFeed("b")}})
	c.Assert(err, qt.IsNil)
	noFeeds, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{})
	c.Assert(err, qt.IsNil)
	node, err := schema.CaptureObject(objectOwner(schema.CoordinationProperties), &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}})
	c.Assert(err, qt.IsNil)
	stopped := nativeFeed("updates")
	stopped.Description.State = native.ChangefeedDescription_STATE_DISABLED
	disabled, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{stopped}})
	c.Assert(err, qt.IsNil)
	for _, row := range []struct {
		name     string
		observed *protocol.Envelope
		want     []string
	}{
		{"a topic with two consumers", topic, []string{"topics=1", "topic_consumers=2"}},
		{"a changefeed set of two feeds, each with two consumers", feeds, []string{"changefeeds=2", "changefeed_consumers=4"}},
		{"a table without changefeeds", noFeeds, []string{"changefeeds=0", "changefeed_consumers=0"}},
		{"a coordination node", node, []string{"coordination_nodes=1"}},
		// Ported from the host's TestReportValues_CountsBothRepresentationsAndDisabledStreams:
		// a disabled changefeed is still a changefeed of the table.
		{"a disabled changefeed with two consumers", disabled, []string{"changefeeds=1", "changefeed_consumers=2"}},
		// Ported from the host's TestExternalReport_CountsObjectsAndColumns.
		{"an external data source", reportedObject(c, schema.ExternalSourceProperties, &Ydb_Table.DescribeExternalDataSourceResult{
			SourceType: new("ObjectStorage"), Location: new(""), Properties: map[string]string{"AUTH_METHOD": "NONE"}}), []string{"external_data_sources=1"}},
		{"an external table of two columns", reportedObject(c, schema.ExternalTableProperties, &native.DescribeExternalTableResult{
			SourceType: new("ObjectStorage"), DataSourcePath: new("/local/app/s3"), Location: new("e/"),
			Columns: []*native.ColumnMeta{{Name: "a", Type: primitive(Ydb.Type_INT64)}, {Name: "b", Type: optional(primitive(Ydb.Type_UTF8))}}}),
			[]string{"external_tables=1", "external_columns=2"}},
		// Ported from the host's TestReplicationReport_CountsEachObject.
		{"an async replication", reportedObject(c, schema.ReplicationProperties, &native.AsyncReplicationSettings{
			Connection: &native.ReplicationConnection{ConnectionString: "grpc://primary:2136/?database=/prod"},
			Items:      []*native.ReplicationItem{{Source: "a", Target: "ra"}}, State: "running"}), []string{"async_replications=1"}},
		{"a transfer", reportedObject(c, schema.TransferProperties, &native.TransferSettings{Source: "tp", Target: "t", Lambda: "($m) -> { return []; }", State: "running"}),
			[]string{"transfers=1"}},
		// Ported from the host's TestSecretReport_CountsEachSecretOnce.
		{"a secret", reportedObject(c, schema.SecretProperties, &native.SecretPresence{}), []string{"secrets=1"}},
		// Ported from the host's TestWorkloadReportsCountCapturedValuesInBothRepresentations.
		{"a resource pool", readPool(c, "batch", workload.PoolSpec{}), []string{"resource_pools=1"}},
		{"a resource pool classifier", readClassifier(c, "route", workload.ClassifierSpec{ResourcePool: "pool", Rank: 20}), []string{"resource_pool_classifiers=1"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed, err := schema.CountValue(row.observed, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			c.Assert(countText(observed), qt.DeepEquals, row.want)
			desired := convertedDesired(c, row.observed)
			counted, err := schema.CountValue(desired, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			c.Assert(countText(counted), qt.DeepEquals, row.want)
		})
	}
}

// reportedObject is a standalone object of kind as inspection captures it
// from /local, in the database context a read carries.
func reportedObject(c *qt.C, kind string, described proto.Message) *protocol.Envelope {
	c.Helper()
	observed, err := schema.CaptureObject(objectOwner(kind), described)
	c.Assert(err, qt.IsNil)
	observed.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
	return observed
}

// convertedDesired converts an observation to its declaration, as the host
// does before it counts a read.
func convertedDesired(c *qt.C, observed *protocol.Envelope) *protocol.Envelope {
	c.Helper()
	converters := map[string]func(*protocol.Envelope, protocol.ValueInput_Representation, protocol.ValueInput_Representation) (*protocol.Envelope, error){
		schema.CoordinationProperties: schema.ConvertCoordination, schema.TableProperties: schema.ConvertTable,
	}
	convert := schema.ConvertNative
	if found, ok := converters[observed.GetKind()]; ok {
		convert = found
	}
	desired, err := convert(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	return desired
}

func TestCountValue_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	topic, err := schema.CaptureObject(topicOwner(), nativeTopic())
	c.Assert(err, qt.IsNil)
	column, err := schema.CaptureColumn(columnOwner(), &native.ColumnMeta{Name: "label", Type: primitive(Ydb.Type_UTF8)})
	c.Assert(err, qt.IsNil)
	forged, err := schema.CaptureObject(topicOwner(), nativeTopic())
	c.Assert(err, qt.IsNil)
	forged.Payload = []byte(`{"consumers":"many"}`)
	for _, row := range []struct {
		name           string
		value          *protocol.Envelope
		representation protocol.ValueInput_Representation
		wantErr        string
	}{
		{"a model no report names", column, protocol.ValueInput_OBSERVED, `the YDB provider counts no YDB column properties`},
		{"a change representation", topic, protocol.ValueInput_CHANGE, `only a desired or observed YDB value is counted`},
		{"an observation counted as a declaration", topic, protocol.ValueInput_DESIRED, `.*`},
		{"a payload that does not validate", forged, protocol.ValueInput_OBSERVED, `invalid YDB observation JSON`},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			counts, err := schema.CountValue(row.value, row.representation)
			c.Assert(err, qt.ErrorMatches, row.wantErr)
			c.Assert(counts, qt.IsNil)
		})
	}
}
