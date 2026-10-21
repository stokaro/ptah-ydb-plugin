package schema_test

import (
	"fmt"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func assertChildRoundTrip(c *qt.C, observed *protocol.Envelope) *protocol.Envelope {
	c.Helper()
	before := proto.CloneOf(observed)
	desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(desired.DefinitionSha256, qt.Equals, schema.NativeDesiredModel(observed.Kind).DefinitionSha256)
	back, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(back, observed), qt.IsTrue)
	c.Assert(proto.Equal(observed, before), qt.IsTrue)
	return desired
}

func TestIndexDeclarationsPreserveEveryKindAndImplementation(t *testing.T) {
	for _, kind := range []string{"globalIndex", "globalAsyncIndex", "globalUniqueIndex", "globalVectorKmeansTreeIndex", "globalFulltextPlainIndex", "globalFulltextRelevanceIndex"} {
		t.Run(kind, func(t *testing.T) {
			c := qt.New(t)
			index := indexObservation("idx")
			c.Assert(protojson.Unmarshal([]byte(fmt.Sprintf(`{"name":"idx","indexColumns":["tenant","label"],"dataColumns":["extra"],"status":"STATUS_READY","%s":{}}`, kind)), index.Description), qt.IsNil)
			names, err := schema.ImplementationNames(index.Description)
			c.Assert(err, qt.IsNil)
			template := index.Tables[0].Description
			template.Attributes = map[string]string{"comment": "Implementation requirements"}
			template.Self = &Ydb_Scheme.Entry{Owner: "owner", EffectivePermissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"read"}}}}
			template.ColumnFamilies = []*Ydb_Table.ColumnFamily{{Name: "cold", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4}}
			template.Columns[0].Family = "cold"
			template.Columns[0].NotNull = proto.Bool(false)
			index.Tables = nil
			for i := len(names) - 1; i >= 0; i-- {
				index.Tables = append(index.Tables, &native.ImplementationTable{Name: names[i], Description: proto.CloneOf(template)})
			}
			observed, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{Indexes: []*native.ObservedIndex{index, indexObservation("a")}})
			c.Assert(err, qt.IsNil)
			desired := assertChildRoundTrip(c, observed)
			var decoded native.IndexSetIntent
			c.Assert(protojson.Unmarshal(desired.Payload, &decoded), qt.IsNil)
			c.Assert(decoded.Indexes[0].Specification.Name, qt.Equals, "a")
			c.Assert(decoded.Indexes[1].Specification.IndexColumns, qt.DeepEquals, []string{"tenant", "label"})
			c.Assert(decoded.Indexes[1].Implementation[0].Columns[0].GetSettings().GetFamily(), qt.Equals, "cold")
		})
	}
}

func TestIndexSequenceProjectionValidatesBeforeExcludingPosition(t *testing.T) {
	c := qt.New(t)
	input := &native.IndexObservations{Indexes: []*native.ObservedIndex{indexObservation("idx")}}
	sequence := &native.SequenceDescription{Name: proto.String("_serial"), DataType: primitive(Ydb.Type_INT64), Increment: proto.Int64(3), Cycle: proto.Bool(false), SetVal: &native.SequenceDescription_SetVal{NextValue: proto.Int64(19)}}
	input.Indexes[0].Tables[0].Description.Columns[1].DefaultValue = &native.ColumnMeta_FromSequence{FromSequence: sequence}
	observed, err := schema.CaptureIndexes(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	desired := assertChildRoundTrip(c, observed)
	var intent native.IndexSetIntent
	c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
	settings := intent.Indexes[0].Implementation[0].Columns[1].Settings.GetSequence()
	c.Assert(settings.GetIncrement(), qt.Equals, int64(3))
	c.Assert(settings.Cycle, qt.IsNotNil)
	c.Assert(settings.GetDataType().GetTypeId(), qt.Equals, Ydb.Type_INT64)
	c.Assert(sequence.GetSetVal().GetNextValue(), qt.Equals, int64(19))
	sequence.SetVal.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 100, protowire.VarintType), 1))
	rejected, err := schema.CaptureIndexes(tableOwner(), input)
	c.Assert(rejected, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}

func TestIndexConversionRefusesPhysicalBoundaries(t *testing.T) {
	c := qt.New(t)
	input := &native.IndexObservations{Indexes: []*native.ObservedIndex{indexObservation("idx")}}
	input.Indexes[0].Tables[0].Description.ShardKeyBounds = []*Ydb.TypedValue{{Type: primitive(Ydb.Type_UINT64), Value: &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: 10}}}}
	observed, err := schema.CaptureIndexes(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(desired, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "index physical shard boundaries cannot become creation settings")
}

func TestTopicDeclarationsPreserveConfigurationAndAccess(t *testing.T) {
	c := qt.New(t)
	input := nativeTopic()
	input.Self.Permissions = []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"write"}}}
	input.Self.EffectivePermissions = []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"read"}}}
	input.PartitioningSettings.MaxActivePartitions = 8
	input.PartitionWriteSpeedBytesPerSecond = 100
	input.PartitionTotalReadSpeedBytesPerSecond = 200
	input.PartitionConsumerReadSpeedBytesPerSecond = 50
	input.PartitionWriteBurstBytes = 300
	input.SupportedCodecs = &Ydb_Topic.SupportedCodecs{Codecs: []int32{1, 2}}
	input.MeteringMode = Ydb_Topic.MeteringMode_METERING_MODE_REQUEST_UNITS
	input.Consumers[0].ReadFrom = timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	input.Consumers[0].AvailabilityPeriod = durationpb.New(time.Hour)
	input.Consumers[0].SupportedCodecs = proto.CloneOf(input.SupportedCodecs)
	observed, err := schema.CaptureObject(topicOwner(), input)
	c.Assert(err, qt.IsNil)
	desired := assertChildRoundTrip(c, observed)
	var intent native.TopicIntent
	c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
	c.Assert(intent.Access.DirectGrants[0].Subject, qt.Equals, "writer")
	c.Assert(intent.Access.RequiredEffectivePermissions[0].Subject, qt.Equals, "reader")
	c.Assert(intent.Consumers[0].StreamingConsumerType, qt.IsNotNil)
	c.Assert(intent.Consumers[1].StreamingConsumerType, qt.IsNil)
}

func TestChangefeedDeclarationsSeparateInitialScanFromProgress(t *testing.T) {
	for _, state := range []native.ChangefeedDescription_State{native.ChangefeedDescription_STATE_ENABLED, native.ChangefeedDescription_STATE_DISABLED, native.ChangefeedDescription_STATE_INITIAL_SCAN} {
		t.Run(state.String(), func(t *testing.T) {
			c := qt.New(t)
			feed := nativeFeed("events")
			feed.Description.State = state
			feed.Description.VirtualTimestamps = true
			feed.Description.AwsRegion = "eu-central-1"
			feed.Description.SchemaChanges = true
			feed.Description.ResolvedTimestampsInterval = durationpb.New(10 * time.Second)
			observed, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{feed}})
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			var intent native.ChangefeedSetIntent
			c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
			c.Assert(intent.Changefeeds[0].InitialScan, qt.IsTrue)
			c.Assert(intent.Changefeeds[0].Disabled, qt.Equals, state == native.ChangefeedDescription_STATE_DISABLED)
			back, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			if state == native.ChangefeedDescription_STATE_INITIAL_SCAN {
				var decoded native.ChangefeedObservations
				c.Assert(protojson.Unmarshal(back.Payload, &decoded), qt.IsNil)
				c.Assert(decoded.Changefeeds[0].Description.State, qt.Equals, native.ChangefeedDescription_STATE_ENABLED)
				c.Assert(decoded.Changefeeds[0].InitialScan, qt.IsTrue)
				c.Assert(decoded.Changefeeds[0].Description.InitialScanProgress, qt.IsNil)
				c.Assert(proto.Equal(back, observed), qt.IsFalse)
				again, err := schema.ConvertNative(back, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(again, desired), qt.IsTrue)
			} else {
				c.Assert(proto.Equal(back, observed), qt.IsTrue)
			}
		})
	}
}

func TestEmptyChildSetsHaveIndependentDesiredContracts(t *testing.T) {
	c := qt.New(t)
	indexes, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{})
	c.Assert(err, qt.IsNil)
	feeds, err := schema.CaptureChangefeeds(tableOwner(), &native.ChangefeedObservations{})
	c.Assert(err, qt.IsNil)
	for _, observed := range []*protocol.Envelope{indexes, feeds} {
		desired := assertChildRoundTrip(c, observed)
		c.Assert(string(desired.Payload), qt.Equals, "{}")
		c.Assert(desired.DefinitionSha256 == observed.DefinitionSha256, qt.IsFalse)
	}
}

func TestDesiredChildrenRefuseIncompleteAndRuntimeSettings(t *testing.T) {
	for _, test := range []struct{ name, kind, payload string }{
		{"null", schema.IndexProperties, `null`},
		{"missing implementation", schema.IndexProperties, `{"indexes":[{"specification":{"name":"idx","indexColumns":["v"],"globalIndex":{}}}]}`},
		{"unknown kind", schema.IndexProperties, `{"indexes":[{"specification":{"name":"idx","indexColumns":["v"],"localIndex":{}}}]}`},
		{"runtime status", schema.IndexProperties, `{"indexes":[{"specification":{"name":"idx","status":"STATUS_READY"}}]}`},
		{"runtime size", schema.IndexProperties, `{"indexes":[{"specification":{"name":"idx","sizeBytes":"1"}}]}`},
		{"nil implementation", schema.IndexProperties, `{"indexes":[{"specification":{"name":"idx"},"implementation":[null]}]}`},
		{"missing partitions", schema.TopicProperties, `{}`},
		{"duplicate consumer", schema.TopicProperties, `{"partitioningSettings":{},"consumers":[{"name":"a"},{"name":"a"}]}`},
		{"null attribute", schema.TopicProperties, `{"partitioningSettings":{},"attributes":{"a":null}}`},
		{"unknown enum", schema.TopicProperties, `{"partitioningSettings":{},"meteringMode":99999}`},
		{"runtime partitions", schema.TopicProperties, `{"partitioningSettings":{},"partitions":[]}`},
		{"runtime consumer stats", schema.TopicProperties, `{"partitioningSettings":{},"consumers":[{"name":"a","consumerStats":{}}]}`},
		{"negative quota", schema.TopicProperties, `{"partitioningSettings":{},"partitionWriteBurstBytes":"-1"}`},
		{"invalid access", schema.TopicProperties, `{"partitioningSettings":{},"access":{"directGrants":[{}]}}`},
		{"missing child topic", schema.ChangefeedProperties, `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON"}]}`},
		{"unknown mode", schema.ChangefeedProperties, `{"changefeeds":[{"name":"f","mode":99999,"format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`},
		{"runtime state", schema.ChangefeedProperties, `{"changefeeds":[{"name":"f","state":"STATE_INITIAL_SCAN"}]}`},
		{"runtime scan progress", schema.ChangefeedProperties, `{"changefeeds":[{"name":"f","initialScanProgress":{}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := schema.NativeDesiredModel(test.kind)
			owner := tableOwner()
			if test.kind == schema.TopicProperties {
				owner = topicOwner()
			}
			value := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: test.kind, Owner: owner, SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}
			result, err := schema.NormalizeDesired(value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

func TestDesiredIndexRefusesIncompleteImplementation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*native.IndexDeclaration)
	}{
		{"missing settings", func(i *native.IndexDeclaration) { i.Implementation[0].Settings = nil }},
		{"missing columns", func(i *native.IndexDeclaration) { i.Implementation[0].Columns = nil }},
		{"missing type", func(i *native.IndexDeclaration) { i.Implementation[0].Columns[0].Type = nil }},
		{"missing column settings", func(i *native.IndexDeclaration) { i.Implementation[0].Columns[0].Settings = nil }},
		{"duplicate column", func(i *native.IndexDeclaration) {
			i.Implementation[0].Columns = append(i.Implementation[0].Columns, proto.CloneOf(i.Implementation[0].Columns[0]))
		}},
		{"missing key column", func(i *native.IndexDeclaration) { i.Implementation[0].Settings.PrimaryKey = []string{"absent"} }},
		{"false default removal", func(i *native.IndexDeclaration) {
			i.Implementation[0].Columns[0].Settings.DefaultValue = &native.ColumnIntent_DropDefault{DropDefault: false}
		}},
		{"unknown implementation", func(i *native.IndexDeclaration) { i.Implementation[0].Name = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			observed, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{Indexes: []*native.ObservedIndex{indexObservation("idx")}})
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			var intent native.IndexSetIntent
			c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
			test.edit(intent.Indexes[0])
			desired.Payload, err = protojson.Marshal(&intent)
			c.Assert(err, qt.IsNil)
			result, err := schema.NormalizeDesired(desired)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}
