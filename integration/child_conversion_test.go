//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func convertNativeProperties(c *qt.C, client *providerruntime.Client, inputs *protocol.NormalizeValuesRequest) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	selected := &protocol.NormalizeValuesRequest{Engine: inputs.Engine, TargetFeatures: inputs.TargetFeatures}
	for _, input := range inputs.Values {
		switch input.Value.Kind {
		case schema.IndexProperties, schema.ChangefeedProperties, schema.TopicProperties, schema.ViewProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties, schema.ColumnStorageProperties, schema.CoordinationProperties:
			selected.Values = append(selected.Values, proto.CloneOf(input))
		}
	}
	c.Assert(len(selected.Values) > 0, qt.IsTrue)
	observed, err := client.NormalizeValues(ctx, selected)
	c.Assert(err, qt.IsNil)
	request := &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: observed.Values, TargetFeatures: inputs.TargetFeatures}
	// Check the public codec too so a sanitized subprocess failure retains a
	// model-specific diagnostic in this fixture without printing schema payloads.
	for _, record := range observed.Values {
		var conversionErr error
		if record.Value.Kind == schema.CoordinationProperties {
			_, conversionErr = schema.ConvertCoordination(record.Value, request.From, request.To)
		} else {
			_, conversionErr = schema.ConvertNative(record.Value, request.From, request.To)
		}
		c.Assert(conversionErr, qt.IsNil, qt.Commentf("native model: %s", record.Value.Kind))
	}
	desired, err := client.ConvertValues(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateConvertedValues(client.Description(), request, desired), qt.IsNil)
	back, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: desired.Values, TargetFeatures: inputs.TargetFeatures})
	c.Assert(err, qt.IsNil)
	for i, value := range desired.Values {
		definition := schema.NativeDesiredModel(value.Value.Kind)
		if value.Value.Kind == schema.CoordinationProperties {
			definition = schema.CoordinationDesiredModel()
		}
		c.Assert(value.Value.DefinitionSha256, qt.Equals, definition.DefinitionSha256)
		expected := proto.CloneOf(observed.Values[i].Value)
		if expected.Kind == schema.ChangefeedProperties {
			var feeds native.ChangefeedObservations
			c.Assert(protojson.Unmarshal(expected.Payload, &feeds), qt.IsNil)
			// A conversion predicts enabled intent; it cannot establish scan completion.
			for _, feed := range feeds.Changefeeds {
				if feed.Description.State == native.ChangefeedDescription_STATE_INITIAL_SCAN {
					feed.Description.State = native.ChangefeedDescription_STATE_ENABLED
				}
			}
			expected.Payload, err = protojson.Marshal(&feeds)
			c.Assert(err, qt.IsNil)
			expected, err = schema.Normalize(expected)
			c.Assert(err, qt.IsNil)
		}
		c.Assert(proto.Equal(expected, back.Values[i].Value), qt.IsTrue)
	}
	again, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: back.Values, TargetFeatures: inputs.TargetFeatures})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(desired, again), qt.IsTrue)
}

func TestChildConversionOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "App", Kind: "table", Name: "Records"}
	indexes, err := schema.CaptureIndexes(owner, &native.IndexObservations{Indexes: []*native.ObservedIndex{{
		Description: &native.TableIndexDescription{Name: "by_label", IndexColumns: []string{"label"}, Status: native.TableIndexDescription_STATUS_READY, Type: &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{}}},
		Tables:      []*native.ImplementationTable{{Name: "indexImplTable", Description: &native.DescribeTableResult{PrimaryKey: []string{"label"}, Columns: []*native.ColumnMeta{{Name: "label", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}, PartitioningSettings: &Ydb_Table.PartitioningSettings{MinPartitionsCount: 4}}}},
	}}})
	c.Assert(err, qt.IsNil)
	topic := &native.DescribeTopicResult{PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 2}, MetricsLevel: proto.Uint32(0), Consumers: []*native.Consumer{{Name: "reader", ConsumerType: &native.Consumer_StreamingConsumerType{StreamingConsumerType: &native.StreamingConsumerType{}}}}}
	feeds, err := schema.CaptureChangefeeds(owner, &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{{Description: &native.ChangefeedDescription{Name: "events", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_INITIAL_SCAN, InitialScanProgress: &native.ChangefeedDescription_InitialScanProgress{PartsTotal: 3, PartsCompleted: 1}}, Topic: topic}}})
	c.Assert(err, qt.IsNil)
	topicOwner := proto.CloneOf(owner)
	topicOwner.Kind = schema.TopicProperties
	topicOwner.Name = "events"
	standalone, err := schema.CaptureObject(topicOwner, topic)
	c.Assert(err, qt.IsNil)
	request := &protocol.NormalizeValuesRequest{Engine: "ydb"}
	for _, value := range []*protocol.Envelope{indexes, feeds, standalone} {
		request.Values = append(request.Values, &protocol.ValueInput{Id: value.Kind, Representation: protocol.ValueInput_OBSERVED, Value: value})
	}
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	convertNativeProperties(c, client, request)
}
