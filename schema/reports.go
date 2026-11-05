package schema

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// reportedMetric is one count metric of a reported model, and what a value of
// the model contributes to it.
type reportedMetric struct {
	name, help string
	// count reads the normalized payload of one value: a desired declaration
	// or an observation, as representation says.
	count func(payload []byte, representation protocol.ValueInput_Representation) (uint64, error)
}

// one counts each value once.
func one([]byte, protocol.ValueInput_Representation) (uint64, error) { return 1, nil }

// reportedKinds are the models whose values `ptah schema stats` counts, with
// the metric names and help texts of the host's built-in YDB engine, so a
// scrape through this provider has the lines a scrape through that engine
// has. Each metric counts what a value holds: a topic is one topic and its
// consumers, a table's changefeed set its feeds and the consumers across them,
// a row table's properties its TTL, its column families and its settings, a
// column table's storage the table and its TTL, and a table's index set its
// global indexes with settings and its vector indexes. Two models count into
// ydb_ttl_tables, as the built-in engine counted a column table whose TTL only
// deletes rows among the tables with a TTL.
var reportedKinds = []struct {
	kind    string
	metrics []reportedMetric
}{
	{TopicProperties, []reportedMetric{{"topics", "Standalone topics", one}, {"topic_consumers", "Consumers of standalone topics", topicConsumers}}},
	{ChangefeedProperties, []reportedMetric{{"changefeeds", "Table changefeeds", changefeedCount}, {"changefeed_consumers", "Consumers of table changefeeds", changefeedConsumers}}},
	{CoordinationProperties, []reportedMetric{{"coordination_nodes", "Standalone coordination nodes", one}}},
	{StreamingQueryProperties, []reportedMetric{{"streaming_queries", "Standalone streaming queries", one}}},
	{ExternalSourceProperties, []reportedMetric{{"external_data_sources", "External data sources", one}}},
	{ExternalTableProperties, []reportedMetric{{"external_tables", "External tables", one}, {"external_columns", "Columns across external tables", externalColumns}}},
	{ReplicationProperties, []reportedMetric{{"async_replications", "Async replications", one}}},
	{TransferProperties, []reportedMetric{{"transfers", "Transfers", one}}},
	{SecretProperties, []reportedMetric{{"secrets", "Secret objects, without their values", one}}},
	{ResourcePoolProperties, []reportedMetric{{"resource_pools", "Resource pools", one}}},
	{ResourcePoolClassifierProperties, []reportedMetric{{"resource_pool_classifiers", "Resource pool classifiers", one}}},
	{TableProperties, []reportedMetric{{"ydb_ttl_tables", ttlTablesHelp, ttlTable},
		{"ydb_column_families", "YDB column families a table states, without a default family that states nothing", statedFamilies},
		{"ydb_table_partitioning", "YDB row tables stating partitioning, read replicas or a key bloom filter", tablePartitioning}}},
	{ColumnStorageProperties, []reportedMetric{{"ydb_ttl_tables", ttlTablesHelp, columnTTLTable},
		{"ydb_column_tables", "YDB tables stored by column", one},
		{"ydb_tiered_ttl_tables", "YDB column tables whose TTL moves rows to an external data source", tieredTTLTable}}},
	{IndexProperties, []reportedMetric{{"ydb_index_partitioning", "YDB global indexes stating partitioning or read replicas", indexPartitioning},
		{"ydb_vector_indexes", "YDB vector indexes with captured settings", vectorIndexes}}},
}

// ttlTablesHelp is the help of the metric two models count into.
const ttlTablesHelp = "Tables with a captured YDB TTL"

// Reports returns the count metrics of the provider's desired and observed
// models, one report per reported model and representation, for a host that
// lists models.report-counts.
func Reports() []*protocol.ModelReport {
	var result []*protocol.ModelReport
	for _, reported := range reportedKinds {
		var metrics []*protocol.ModelMetric
		for _, metric := range reported.metrics {
			metrics = append(metrics, &protocol.ModelMetric{Name: metric.name, Help: metric.help})
		}
		for _, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
			result = append(result, &protocol.ModelReport{Kind: reported.kind, Representation: representation, Metrics: metrics})
		}
	}
	return result
}

// CountValue validates one desired or observed value as normalization does
// and returns its counts, one for each metric of its model's report, in the
// declared order. A value of a model no report names, or one that does not
// validate, is refused.
func CountValue(value *protocol.Envelope, representation protocol.ValueInput_Representation) ([]*protocol.MetricCount, error) {
	var normalized *protocol.Envelope
	var err error
	switch representation {
	case protocol.ValueInput_DESIRED:
		normalized, err = NormalizeDesired(value)
	case protocol.ValueInput_OBSERVED:
		normalized, err = Normalize(value)
	default:
		return nil, fmt.Errorf("only a desired or observed YDB value is counted")
	}
	if err != nil {
		return nil, err
	}
	for _, reported := range reportedKinds {
		if reported.kind != normalized.GetKind() {
			continue
		}
		counts := make([]*protocol.MetricCount, 0, len(reported.metrics))
		for _, metric := range reported.metrics {
			count, err := metric.count(normalized.GetPayload(), representation)
			if err != nil {
				return nil, err
			}
			counts = append(counts, &protocol.MetricCount{Name: metric.name, Value: count})
		}
		return counts, nil
	}
	return nil, fmt.Errorf("the YDB provider counts no %s", modelDisplayName(normalized.GetKind()))
}

func topicConsumers(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	if representation == protocol.ValueInput_DESIRED {
		var topic native.TopicIntent
		err := unmarshalCounted(payload, &topic)
		return uint64(len(topic.GetConsumers())), err
	}
	var topic native.DescribeTopicResult
	err := unmarshalCounted(payload, &topic)
	return uint64(len(topic.GetConsumers())), err
}

func changefeedCount(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	feeds, err := changefeedTopics(payload, representation)
	return uint64(len(feeds)), err
}

func changefeedConsumers(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	feeds, err := changefeedTopics(payload, representation)
	var total uint64
	for _, consumers := range feeds {
		total += uint64(consumers) // #nosec G115 -- a count of consumers, never negative
	}
	return total, err
}

// changefeedTopics returns the number of consumers of each feed of a table's
// changefeed set.
func changefeedTopics(payload []byte, representation protocol.ValueInput_Representation) ([]int, error) {
	var consumers []int
	if representation == protocol.ValueInput_DESIRED {
		var set native.ChangefeedSetIntent
		if err := unmarshalCounted(payload, &set); err != nil {
			return nil, err
		}
		for _, feed := range set.GetChangefeeds() {
			consumers = append(consumers, len(feed.GetTopic().GetConsumers()))
		}
		return consumers, nil
	}
	var set native.ChangefeedObservations
	if err := unmarshalCounted(payload, &set); err != nil {
		return nil, err
	}
	for _, feed := range set.GetChangefeeds() {
		consumers = append(consumers, len(feed.GetTopic().GetConsumers()))
	}
	return consumers, nil
}

func externalColumns(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	if representation == protocol.ValueInput_DESIRED {
		var table native.ExternalTableIntent
		err := unmarshalCounted(payload, &table)
		return uint64(len(table.GetColumns())), err
	}
	var table native.DescribeExternalTableResult
	err := unmarshalCounted(payload, &table)
	return uint64(len(table.GetColumns())), err
}

// unmarshalCounted reads a payload normalization already accepted.
func unmarshalCounted(payload []byte, target proto.Message) error {
	if err := protojson.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("invalid normalized YDB value JSON")
	}
	return nil
}
