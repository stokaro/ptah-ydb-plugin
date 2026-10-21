//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestNativeStreamsSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var version string
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
	table := path.Join(database, directory, "records")
	options := "MODE='NEW_AND_OLD_IMAGES', FORMAT='JSON', RETENTION_PERIOD=Interval('PT12H'), INITIAL_SCAN=TRUE, VIRTUAL_TIMESTAMPS=TRUE, RESOLVED_TIMESTAMPS=Interval('PT10S')"
	newer := !strings.HasPrefix(version, "25.1.")
	if newer {
		options += ", USER_SIDS=TRUE, SCHEMA_CHANGES=TRUE"
	}
	err := execFixtureSchema(ctx, opened.DB, "ALTER TABLE `"+table+"` ADD CHANGEFEED z_events WITH ("+options+")")
	c.Assert(err, qt.IsNil)
	err = execFixtureSchema(ctx, opened.DB, "ALTER TABLE `"+table+"` ADD CHANGEFEED a_audit WITH (MODE='NEW_AND_OLD_IMAGES',FORMAT='DEBEZIUM_JSON')")
	c.Assert(err, qt.IsNil)
	err = execFixtureSchema(ctx, opened.DB, "ALTER TOPIC `"+table+"/z_events` ADD CONSUMER z_plain, ADD CONSUMER a_billing WITH (important=TRUE,read_from=Timestamp('2026-01-01T00:00:00Z'),supported_codecs='raw,gzip')")
	c.Assert(err, qt.IsNil)
	topicPath := path.Join(database, directory, "order.events")
	_, err = opened.DB.ExecContext(ctx, "CREATE TOPIC `"+topicPath+"` (CONSUMER z_plain,CONSUMER a_billing WITH (important=TRUE)) WITH (min_active_partitions=2,retention_period=Interval('PT36H'),partition_write_speed_bytes_per_second=2097152,partition_write_burst_bytes=3145728,supported_codecs='raw,gzip')")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := opened.DB.ExecContext(cleanup, "DROP TOPIC `"+topicPath+"`")
		c.Check(err, qt.IsNil)
	})
	direct, err := inspect.Read(ctx, opened.Driver, database, []string{directory})
	c.Assert(err, qt.IsNil)
	c.Assert(direct.Tables, qt.HasLen, 1)
	c.Assert(direct.NativeObjects, qt.HasLen, 1)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.NativeObjects, qt.HasLen, 1)
	observedTopic := snapshot.NativeObjects[0]
	c.Assert(observedTopic.Kind, qt.Equals, schema.TopicProperties)
	c.Assert(observedTopic.Owner.Name, qt.Equals, "order.events")
	c.Assert(observedTopic.Owner.Schema, qt.Equals, directory)
	var topic native.DescribeTopicResult
	c.Assert(protojson.Unmarshal(observedTopic.Payload, &topic), qt.IsNil)
	c.Assert(topic.GetPartitioningSettings().GetMinActivePartitions(), qt.Equals, int64(2))
	c.Assert(topic.GetRetentionPeriod().GetSeconds(), qt.Equals, int64(36*3600))
	c.Assert(topic.PartitionWriteSpeedBytesPerSecond, qt.Equals, int64(2097152))
	c.Assert(topic.PartitionWriteBurstBytes, qt.Equals, int64(3145728))
	c.Assert(topic.GetSupportedCodecs().GetCodecs(), qt.DeepEquals, []int32{1, 2})
	c.Assert(topic.Partitions, qt.HasLen, 0)
	c.Assert(topic.Consumers, qt.HasLen, 2)
	c.Assert(topic.Consumers[0].Name, qt.Equals, "a_billing")
	c.Assert(topic.Consumers[0].Important, qt.IsTrue)
	c.Assert(topic.GetSelf().GetName(), qt.Equals, "")
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	c.Assert(snapshot.Tables[0].Facets, qt.HasLen, 3)
	facet := snapshot.Tables[0].Facets[2]
	c.Assert(facet.Kind, qt.Equals, schema.ChangefeedProperties)
	var feeds native.ChangefeedObservations
	c.Assert(protojson.Unmarshal(facet.Payload, &feeds), qt.IsNil)
	c.Assert(feeds.Changefeeds, qt.HasLen, 2)
	c.Assert(feeds.Changefeeds[0].Description.Name, qt.Equals, "a_audit")
	c.Assert(feeds.Changefeeds[0].Description.Format, qt.Equals, Ydb_Table.ChangefeedFormat_FORMAT_DEBEZIUM_JSON)
	c.Assert(feeds.Changefeeds[0].InitialScan, qt.IsFalse)
	events := feeds.Changefeeds[1]
	c.Assert(events.Description.Name, qt.Equals, "z_events")
	c.Assert(events.Description.Mode, qt.Equals, Ydb_Table.ChangefeedMode_MODE_NEW_AND_OLD_IMAGES)
	c.Assert(events.Description.VirtualTimestamps, qt.IsTrue)
	c.Assert(events.Description.GetResolvedTimestampsInterval().GetSeconds(), qt.Equals, int64(10))
	c.Assert(events.Description.UserSids, qt.Equals, newer)
	c.Assert(events.Description.SchemaChanges, qt.Equals, newer)
	c.Assert(events.InitialScan, qt.IsTrue)
	c.Assert(events.Description.InitialScanProgress, qt.IsNil)
	c.Assert(events.Topic.GetRetentionPeriod().GetSeconds(), qt.Equals, int64(12*3600))
	c.Assert(events.Topic.Consumers, qt.HasLen, 2)
	billing := events.Topic.Consumers[0]
	c.Assert(billing.Name, qt.Equals, "a_billing")
	c.Assert(billing.Important, qt.IsTrue)
	c.Assert(billing.GetReadFrom().AsTime().Format(time.RFC3339), qt.Equals, "2026-01-01T00:00:00Z")
	c.Assert(billing.GetSupportedCodecs().GetCodecs(), qt.DeepEquals, []int32{1, 2})
	c.Assert(billing.Attributes["_service_type"], qt.Equals, "data-streams")
	if newer {
		c.Assert(billing.GetStreamingConsumerType(), qt.IsNotNil)
	}
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	normalized, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
	again, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, normalized), qt.IsTrue)
	convertNativeProperties(c, client, values)
}
