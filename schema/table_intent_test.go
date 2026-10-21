package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestTableConversionPreservesConfigurationAndAccessRequirements(t *testing.T) {
	c := qt.New(t)
	original := &native.DescribeTableResult{
		PrimaryKey: []string{"tenant", "id"},
		Self: &Ydb_Scheme.Entry{Owner: "alice", Permissions: []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"ydb.generic.write"}}},
			EffectivePermissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.generic.read"}}, {Subject: "writer", PermissionNames: []string{"ydb.generic.write"}}}},
		TtlSettings:          &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "expires", ExpireAfterSeconds: 86400}}},
		StorageSettings:      &Ydb_Table.StorageSettings{TabletCommitLog0: &Ydb_Table.StoragePool{Media: "ssd"}, TabletCommitLog1: &Ydb_Table.StoragePool{Media: "ssd"}, External: &Ydb_Table.StoragePool{Media: "hdd"}},
		ColumnFamilies:       []*Ydb_Table.ColumnFamily{{Name: "cold", Data: &Ydb_Table.StoragePool{Media: "hdd"}, Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4}, {Name: "default", Data: &Ydb_Table.StoragePool{Media: "ssd"}}},
		Attributes:           map[string]string{"comment": "Case-sensitive caption", "custom": ""},
		PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitionBy: []string{"tenant"}, PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 2, MaxPartitionsCount: 16},
		KeyBloomFilter:       Ydb.FeatureFlag_ENABLED, ReadReplicasSettings: &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount{AnyAzReadReplicasCount: 2}},
		Tiering: "archive", Temporary: true,
	}
	observed, err := schema.CaptureTable(tableOwner(), original)
	c.Assert(err, qt.IsNil)
	before := proto.CloneOf(observed)
	desired, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(desired.DefinitionSha256, qt.Equals, schema.TableDesiredModel().DefinitionSha256)
	var intent native.RowTableIntent
	c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
	c.Assert(intent.PrimaryKey, qt.DeepEquals, []string{"tenant", "id"})
	c.Assert(intent.Access.GetOwner(), qt.Equals, "alice")
	c.Assert(intent.Access.DirectGrants, qt.HasLen, 1)
	c.Assert(intent.Access.DirectGrants[0].Subject, qt.Equals, "writer")
	c.Assert(intent.Access.RequiredEffectivePermissions, qt.HasLen, 2)
	c.Assert(intent.Access.RequiredEffectivePermissions[0].Subject, qt.Equals, "reader")
	c.Assert(intent.ColumnFamilies[0].Name, qt.Equals, "cold")
	c.Assert(intent.ColumnFamilies[1].Name, qt.Equals, "default")
	c.Assert(intent.Attributes, qt.DeepEquals, original.Attributes)
	back, err := schema.ConvertTable(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(back, observed), qt.IsTrue)
	c.Assert(proto.Equal(observed, before), qt.IsTrue)
	c.Assert(original.StoreType, qt.Equals, Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED)
}

func TestTableConversionKeepsNativeDefaultsAndEpochTTL(t *testing.T) {
	for _, test := range []struct {
		name  string
		table *native.DescribeTableResult
	}{
		{"native defaults", &native.DescribeTableResult{PrimaryKey: []string{"id"}}},
		{"epoch TTL", &native.DescribeTableResult{PrimaryKey: []string{"id"}, TtlSettings: &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_ValueSinceUnixEpoch{ValueSinceUnixEpoch: &Ydb_Table.ValueSinceUnixEpochModeSettings{ColumnName: "expiry", ColumnUnit: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MILLISECONDS, ExpireAfterSeconds: 42}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			observed, err := schema.CaptureTable(tableOwner(), test.table)
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			back, err := schema.ConvertTable(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(back, observed), qt.IsTrue)
		})
	}
}

func TestTableConversionRefusesPhysicalTopology(t *testing.T) {
	c := qt.New(t)
	observed, err := schema.CaptureTable(tableOwner(), &native.DescribeTableResult{PrimaryKey: []string{"id"}, ShardKeyBounds: []*Ydb.TypedValue{{Type: primitive(Ydb.Type_UINT64), Value: &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: 100}}}}})
	c.Assert(err, qt.IsNil)
	result, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "YDB physical shard boundaries cannot become schema split points")
}

func TestDesiredTableRefusesIncompleteOrAmbiguousSettings(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"missing key", `{}`},
		{"null document", `null`},
		{"duplicate key", `{"primaryKey":["id","id"]}`},
		{"common columns", `{"primaryKey":["id"],"columns":[]}`},
		{"statistics", `{"primaryKey":["id"],"tableStats":{}}`},
		{"runtime topology", `{"primaryKey":["id"],"shardKeyBounds":[]}`},
		{"observed access", `{"primaryKey":["id"],"self":{}}`},
		{"null attribute", `{"primaryKey":["id"],"attributes":{"comment":null}}`},
		{"null family", `{"primaryKey":["id"],"columnFamilies":[null]}`},
		{"duplicate attribute", `{"primaryKey":["id"],"attributes":{"a":"one","a":"two"}}`},
		{"unknown flag", `{"primaryKey":["id"],"keyBloomFilter":9999}`},
		{"duplicate family", `{"primaryKey":["id"],"columnFamilies":[{"name":"cold"},{"name":"cold"}]}`},
		{"missing TTL mode", `{"primaryKey":["id"],"ttlSettings":{}}`},
		{"missing TTL column", `{"primaryKey":["id"],"ttlSettings":{"dateTypeColumn":{}}}`},
		{"missing epoch unit", `{"primaryKey":["id"],"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"expiry"}}}`},
		{"reversed partition bounds", `{"primaryKey":["id"],"partitioningSettings":{"minPartitionsCount":"3","maxPartitionsCount":"2"}}`},
		{"unnamed grant", `{"primaryKey":["id"],"access":{"directGrants":[{"permissionNames":["ydb.generic.read"]}]}}`},
		{"duplicate effective permission", `{"primaryKey":["id"],"access":{"requiredEffectivePermissions":[{"subject":"r","permissionNames":["read","read"]}]}}`},
		{"invented inheritance", `{"primaryKey":["id"],"access":{"inheritance":false}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := schema.TableDesiredModel()
			input := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}
			result, err := schema.NormalizeDesired(input)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}
