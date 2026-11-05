package schema_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
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
	for _, row := range []struct {
		name     string
		observed *protocol.Envelope
		want     []string
	}{
		{"a topic with two consumers", topic, []string{"topics=1", "topic_consumers=2"}},
		{"a changefeed set of two feeds, each with two consumers", feeds, []string{"changefeeds=2", "changefeed_consumers=4"}},
		{"a table without changefeeds", noFeeds, []string{"changefeeds=0", "changefeed_consumers=0"}},
		{"a coordination node", node, []string{"coordination_nodes=1"}},
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

// convertedDesired converts an observation to its declaration, as the host
// does before it counts a read.
func convertedDesired(c *qt.C, observed *protocol.Envelope) *protocol.Envelope {
	c.Helper()
	convert := schema.ConvertNative
	if observed.GetKind() == schema.CoordinationProperties {
		convert = schema.ConvertCoordination
	}
	desired, err := convert(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	return desired
}

func TestCountValue_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	topic, err := schema.CaptureObject(topicOwner(), nativeTopic())
	c.Assert(err, qt.IsNil)
	indexes, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{})
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
		{"a model no report names", indexes, protocol.ValueInput_OBSERVED, `the YDB provider counts no YDB index sets`},
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
