package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func TestTableConversionPreservesConfigurationAndAccessRequirements(t *testing.T) {
	c := testcheck.New(t)
	original := &native.DescribeTableResult{
		PrimaryKey: []string{"tenant", "id"},
		Self: &Ydb_Scheme.Entry{Owner: "alice", Permissions: []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"ydb.generic.write"}}},
			EffectivePermissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.generic.read"}}, {Subject: "writer", PermissionNames: []string{"ydb.generic.write"}}}},
		TtlSettings:          &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "expires", ExpireAfterSeconds: 86400}}},
		StorageSettings:      &Ydb_Table.StorageSettings{TabletCommitLog0: &Ydb_Table.StoragePool{Media: "ssd"}, TabletCommitLog1: &Ydb_Table.StoragePool{Media: "ssd"}, External: &Ydb_Table.StoragePool{Media: "hdd"}},
		ColumnFamilies:       []*native.ColumnFamily{{Name: "cold", Data: &Ydb_Table.StoragePool{Media: "hdd"}, Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4, KeepInMemory: Ydb.FeatureFlag_ENABLED, CacheMode: native.ColumnFamily_CACHE_MODE_IN_MEMORY}, {Name: "default", Data: &Ydb_Table.StoragePool{Media: "ssd"}}},
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
	// A family the table keeps in memory is declared so: ported from the host's
	// TestColumnFamiliesConvertFeatures_ProjectsEachRepresentation and
	// TestColumnFamilies_KeepInMemoryTravelsWithTheRead.
	c.Assert(intent.ColumnFamilies[0].KeepInMemory, qt.Equals, Ydb.FeatureFlag_ENABLED)
	c.Assert(intent.ColumnFamilies[1].Name, qt.Equals, "default")
	c.Assert(intent.Attributes, qt.DeepEquals, original.Attributes)
	// The settings YDB gives every new table are left out, as the host's
	// embedded engine wrote a read against them; the others stay.
	c.Assert(proto.Equal(intent.PartitioningSettings, &Ydb_Table.PartitioningSettings{PartitionBy: []string{"tenant"}, MinPartitionsCount: 2, MaxPartitionsCount: 16}),
		qt.IsTrue, qt.Commentf("%v", intent.PartitioningSettings))
	c.Assert(intent.KeyBloomFilter, qt.Equals, Ydb.FeatureFlag_ENABLED)
	back, err := schema.ConvertTable(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	again, err := schema.ConvertTable(back, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, desired), qt.IsTrue)
	c.Assert(proto.Equal(observed, before), qt.IsTrue)
	c.Assert(original.StoreType, qt.Equals, Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED)
}

// YDB describes a subject once for each access entry it keeps, so a GRANT of
// two permissions reads as two grants to one subject, and a grant on the
// directory above adds one more among the effective permissions. The desired
// table names each subject once, with every permission its entries name, in
// the order YDB described them.
func TestTableConversionMergesTheEntriesOfOneSubject_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	grant := func(subject string, names ...string) *Ydb_Scheme.Permissions {
		return &Ydb_Scheme.Permissions{Subject: subject, PermissionNames: names}
	}
	observed, err := schema.CaptureTable(tableOwner(), &native.DescribeTableResult{PrimaryKey: []string{"id"}, Self: &Ydb_Scheme.Entry{
		Permissions: []*Ydb_Scheme.Permissions{grant("writer", "ydb.granular.update_row"), grant("reader", "ydb.granular.select_row"),
			grant("writer", "ydb.granular.select_row")},
		EffectivePermissions: []*Ydb_Scheme.Permissions{grant("USERS", "ydb.database.connect"), grant("writer", "ydb.granular.select_row"),
			grant("writer", "ydb.granular.update_row"), grant("writer", "ydb.granular.select_row")},
	}})
	c.Assert(err, qt.IsNil)

	desired, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)

	c.Assert(err, qt.IsNil)
	var intent native.RowTableIntent
	c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
	c.Assert(intent.Access.DirectGrants, qt.CmpEquals(protocmp.Transform()), []*Ydb_Scheme.Permissions{
		grant("writer", "ydb.granular.update_row", "ydb.granular.select_row"), grant("reader", "ydb.granular.select_row"),
	})
	c.Assert(intent.Access.RequiredEffectivePermissions, qt.CmpEquals(protocmp.Transform()), []*Ydb_Scheme.Permissions{
		grant("USERS", "ydb.database.connect"), grant("writer", "ydb.granular.select_row", "ydb.granular.update_row"),
	})
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
			c := testcheck.New(t)
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
	c := testcheck.New(t)
	observed, err := schema.CaptureTable(tableOwner(), &native.DescribeTableResult{PrimaryKey: []string{"id"}, ShardKeyBounds: []*Ydb.TypedValue{{Type: primitive(Ydb.Type_UINT64), Value: &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: 100}}}}})
	c.Assert(err, qt.IsNil)
	result, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "YDB physical shard boundaries cannot become schema split points")
}

func TestDesiredTableRefusesIncompleteOrAmbiguousSettings(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"null document", `null`},
		{"blank key column", `{"primaryKey":[""]}`},
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
		// Ported from the host's TestColumnFamiliesCodecs_RefuseMalformedValues: a family
		// lists no columns, which each column names itself, and YDB has no zstd.
		{"a family listing columns", `{"primaryKey":["id"],"columnFamilies":[{"name":"cold","columns":[]}]}`},
		{"a compression YDB lacks", `{"primaryKey":["id"],"columnFamilies":[{"name":"cold","compression":"COMPRESSION_ZSTD"}]}`},
		{"missing TTL mode", `{"primaryKey":["id"],"ttlSettings":{}}`},
		{"missing TTL column", `{"primaryKey":["id"],"ttlSettings":{"dateTypeColumn":{}}}`},
		{"missing epoch unit", `{"primaryKey":["id"],"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"expiry"}}}`},
		{"reversed partition bounds", `{"primaryKey":["id"],"partitioningSettings":{"minPartitionsCount":"3","maxPartitionsCount":"2"}}`},
		{"unnamed grant", `{"primaryKey":["id"],"access":{"directGrants":[{"permissionNames":["ydb.generic.read"]}]}}`},
		{"duplicate effective permission", `{"primaryKey":["id"],"access":{"requiredEffectivePermissions":[{"subject":"r","permissionNames":["read","read"]}]}}`},
		{"invented inheritance", `{"primaryKey":["id"],"access":{"inheritance":false}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			definition := schema.TableDesiredModel()
			input := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}
			result, err := schema.NormalizeDesired(input)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// A source that states settings knows no columns, so it states no key: the
// effective key comes from the common columns.
func TestDesiredTableAcceptsSettingsWithoutAKey(t *testing.T) {
	c := testcheck.New(t)
	definition := schema.TableDesiredModel()
	input := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: []byte(`{"keyBloomFilter":"ENABLED"}`)}
	result, err := schema.NormalizeDesired(input)
	c.Assert(err, qt.IsNil)
	c.Assert(string(result.Payload), qt.Equals, `{"keyBloomFilter":"ENABLED"}`)
}

// A declared starting layout leaves the table with the minimum partition
// count YDB sets from it, unless the declaration states a minimum of its own,
// so the observation predicted from the declaration holds that minimum. The
// host compares a document with itself through this prediction. Ported from
// the host's TestCompare_YDBTablePartitioning_SameDocument.
func TestTableConversionPredictsTheMinimumALayoutSets(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		want          uint64
	}{
		{"a uniform layout", `{"primaryKey":["id"],"uniformPartitions":"4"}`, 4},
		{"a uniform layout beside a stated minimum", `{"primaryKey":["id"],"uniformPartitions":"4","partitioningSettings":{"minPartitionsCount":"2"}}`, 2},
		{"no layout", `{"primaryKey":["id"]}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			definition := schema.TableDesiredModel()
			declared := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion,
				Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}

			observed, err := schema.ConvertTable(declared, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)

			c.Assert(err, qt.IsNil)
			var table native.DescribeTableResult
			c.Assert(protojson.Unmarshal(observed.Payload, &table), qt.IsNil)
			c.Assert(table.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, test.want)
		})
	}
}

// Without a key there is no observation to predict, so the conversion says so.
func TestDesiredTableWithoutAKeyIsNotAnObservation(t *testing.T) {
	c := testcheck.New(t)
	definition := schema.TableDesiredModel()
	input := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: []byte(`{"keyBloomFilter":"ENABLED"}`)}
	result, err := schema.ConvertTable(input, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "observed YDB table properties require a primary key")
}

// A read reports store_external_blobs DISABLED for every row table that sets
// nothing; the declaration converted from it states no storage settings. An
// explicit setting stays.
func TestTableConversionLeavesDefaultStorageOut(t *testing.T) {
	for _, test := range []struct {
		name    string
		storage *Ydb_Table.StorageSettings
		stated  bool
	}{
		{"the server's default", &Ydb_Table.StorageSettings{StoreExternalBlobs: Ydb.FeatureFlag_DISABLED}, false},
		{"external blobs", &Ydb_Table.StorageSettings{StoreExternalBlobs: Ydb.FeatureFlag_ENABLED}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed, err := schema.CaptureTable(tableOwner(), &native.DescribeTableResult{PrimaryKey: []string{"id"}, StorageSettings: test.storage})
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertTable(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			var intent native.RowTableIntent
			c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
			c.Assert(intent.StorageSettings != nil, qt.Equals, test.stated)
		})
	}
}
