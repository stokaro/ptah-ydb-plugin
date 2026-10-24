package columnstore_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

//go:embed testdata/25-1.json
var oldDescription []byte

//go:embed testdata/26-2-indexes.json
var indexDescription []byte

//go:embed testdata/26-2-inline-indexes.json
var inlineDescription []byte

//go:embed testdata/26-2-ttl.json
var ttlDescription []byte

//go:embed testdata/26-2-epoch.json
var epochDescription []byte

func column(name string, typ Ydb.Type_PrimitiveTypeId, nullable bool) *native.ColumnMeta {
	value := &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: typ}}
	if nullable {
		value = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: value}}}
	}
	return &native.ColumnMeta{Name: name, Type: value}
}

// table writes the table service's description of a column table hashed by id.
func table(name string, columns ...*native.ColumnMeta) *native.ColumnTableDescribeResult {
	return &native.ColumnTableDescribeResult{
		Self:       &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		StoreType:  Ydb_Table.StoreType_STORE_TYPE_COLUMN,
		PrimaryKey: []string{"id"}, Columns: columns,
		PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitionBy: []string{"id"}},
	}
}

func indexTable() *native.ColumnTableDescribeResult {
	return table("col_index_probe", column("id", Ydb.Type_UINT64, false), column("body", Ydb.Type_UTF8, true))
}

// tieredTable is the table service's description of the TTL fixtures: two
// tiers on column at, the first moving rows to /local/col_archive.
func tieredTable(name string, typ Ydb.Type_PrimitiveTypeId, unit Ydb_Table.ValueSinceUnixEpochModeSettings_Unit) *native.ColumnTableDescribeResult {
	described := table(name, column("at", typ, false), column("id", Ydb.Type_UINT64, false))
	described.PrimaryKey = []string{"at", "id"}
	expression := func(seconds uint32) *native.TtlTier {
		if unit == Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_UNSPECIFIED {
			return &native.TtlTier{Expression: &native.TtlTier_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "at", ExpireAfterSeconds: seconds}}}
		}
		return &native.TtlTier{Expression: &native.TtlTier_ValueSinceUnixEpoch{ValueSinceUnixEpoch: &Ydb_Table.ValueSinceUnixEpochModeSettings{ColumnName: "at", ColumnUnit: unit, ExpireAfterSeconds: seconds}}}
	}
	evict, remove := expression(86400), expression(604800)
	evict.Action = &native.TtlTier_EvictToExternalStorage{EvictToExternalStorage: &native.EvictionToExternalStorageSettings{Storage: "/local/col_archive"}}
	remove.Action = &native.TtlTier_Delete{Delete: &emptypb.Empty{}}
	described.TtlSettings = &native.TtlSettings{Mode: &native.TtlSettings_TieredTtl{TieredTtl: &native.TieredModeSettings{Tiers: []*native.TtlTier{evict, remove}}}}
	return described
}

func TestCapturedDescriptionsPreserveStorage(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       []byte
		table      *native.ColumnTableDescribeResult
		partitions uint64
		indexes    int
	}{
		{"old", oldDescription, table("col_probe", column("id", Ydb.Type_UINT64, false), column("body", Ydb.Type_UTF8, true), column("at", Ydb.Type_TIMESTAMP, false)), 64, 0},
		{"indexes", indexDescription, indexTable(), 64, 5},
		{"inline", inlineDescription, table("ptah_inline_probe", column("id", Ydb.Type_UINT64, false), column("body", Ydb.Type_UTF8, true)), 1, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := columnstore.Decode(test.body, "/local/"+test.table.Self.Name, test.table)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Partitions, qt.Equals, test.partitions)
			c.Assert(result.DataChannelCount, qt.Equals, uint64(64))
			c.Assert(result.HashColumns, qt.DeepEquals, []string{"id"})
			c.Assert(result.LocalIndexes, qt.HasLen, test.indexes)
			c.Assert(result.Ttl, qt.IsNil)
		})
	}
	c := qt.New(t)
	result, err := columnstore.Decode(indexDescription, "/local/col_index_probe", indexTable())
	c.Assert(err, qt.IsNil)
	c.Assert(result.LocalIndexes[0].Name, qt.Equals, "bf")
	c.Assert(result.LocalIndexes[0].Columns, qt.DeepEquals, []string{"body"})
	c.Assert(result.LocalIndexes[0].GetBloom().GetFalsePositiveProbability(), qt.Equals, 0.01)
	c.Assert(result.LocalIndexes[0].InheritPortionStorage, qt.IsTrue)
	c.Assert(result.LocalIndexes[2].Name, qt.Equals, "mm")
	c.Assert(result.LocalIndexes[2].StorageId, qt.Equals, "__LOCAL_METADATA")
	c.Assert(result.LocalIndexes[3].GetNgram().GetNgramSize(), qt.Equals, uint64(3))
	c.Assert(result.LocalIndexes[4].GetNgram().NgramSize, qt.IsNil)
	c.Assert(result.LocalIndexes[4].GetNgram().FalsePositiveProbability, qt.IsNil)
	c.Assert(result.LocalIndexes[4].GetNgram().CaseSensitive, qt.IsNotNil)
}

func TestCapturedTTLRetainsUnitsActionsAndPresence(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          []byte
		typ           Ydb.Type_PrimitiveTypeId
		unit          Ydb_Table.ValueSinceUnixEpochModeSettings_Unit
		hasExpiration bool
	}{
		{"col_tier_probe", ttlDescription, Ydb.Type_TIMESTAMP, Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_UNSPECIFIED, true},
		{"col_epoch_probe", epochDescription, Ydb.Type_UINT64, Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := columnstore.Decode(test.body, "/local/"+test.name, tieredTable(test.name, test.typ, test.unit))
			c.Assert(err, qt.IsNil)
			ttl := result.Ttl.GetEnabled()
			c.Assert(ttl.Column, qt.Equals, "at")
			c.Assert(ttl.ColumnUnit, qt.Equals, uint64(test.unit))
			c.Assert(ttl.ExpireAfterSeconds != nil, qt.Equals, test.hasExpiration)
			c.Assert(ttl.Tiers, qt.HasLen, 2)
			c.Assert(ttl.Tiers[0].ApplyAfterSeconds, qt.Equals, uint64(86400))
			c.Assert(ttl.Tiers[0].GetExternalStorage(), qt.Equals, "/local/col_archive")
			c.Assert(ttl.Tiers[1].ApplyAfterSeconds, qt.Equals, uint64(604800))
			c.Assert(ttl.Tiers[1].GetDelete(), qt.IsNotNil)
		})
	}
}

// captured reads a pair of descriptions one live server answered for one
// table: the monitoring page, unedited, and the table service's description as
// protojson. testdata/README.md says how each table was made.
func captured(c *qt.C, name string) ([]byte, *native.ColumnTableDescribeResult) {
	c.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	c.Assert(err, qt.IsNil)
	encoded, err := os.ReadFile(filepath.Join("testdata", name+".table.json"))
	c.Assert(err, qt.IsNil)
	var described native.ColumnTableDescribeResult
	c.Assert(protojson.Unmarshal(encoded, &described), qt.IsNil)
	return body, &described
}

// tier is one TTL tier as the tests compare it: the seconds, and the external
// data source it moves rows to, "" for a tier that deletes.
type tier struct {
	Seconds uint64
	Storage string
}

// policy is a TTL as the tests compare it.
type policy struct {
	Column string
	Unit   uint64
	Tiers  []tier
}

func policyOf(ttl *native.ColumnTTLPolicy) policy {
	enabled := ttl.GetEnabled()
	if enabled == nil {
		return policy{}
	}
	result := policy{Column: enabled.Column, Unit: enabled.ColumnUnit}
	for _, step := range enabled.Tiers {
		result.Tiers = append(result.Tiers, tier{Seconds: step.ApplyAfterSeconds, Storage: step.GetExternalStorage()})
	}
	return result
}

// TestCapturedLivePairs decodes the pairs both lines answered: the shard count
// a table was created with, a hash key other than the primary key, a TTL that
// only deletes set in CREATE TABLE and by ALTER TABLE, and tiered TTLs on a
// date and on an epoch column.
func TestCapturedLivePairs(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		hash       []string
		partitions uint64
		ttl        policy
	}{
		{name: "25-1-hash", path: "/local/probe/c1", hash: []string{"id"}, partitions: 3},
		{name: "26-2-hash", path: "/local/probe/c1", hash: []string{"id"}, partitions: 3},
		{name: "25-1-delete-ttl-inline", path: "/local/probe/c3", hash: []string{"ts", "id"}, partitions: 64,
			ttl: policy{Column: "ts", Tiers: []tier{{Seconds: 86400}}}},
		{name: "26-2-delete-ttl-inline", path: "/local/probe/c6", hash: []string{"id"}, partitions: 2,
			ttl: policy{Column: "ts", Tiers: []tier{{Seconds: 3600}}}},
		{name: "26-2-delete-ttl", path: "/local/probe/c3", hash: []string{"id"}, partitions: 2,
			ttl: policy{Column: "ts", Tiers: []tier{{Seconds: 7200}}}},
		{name: "26-2-tiered-inline", path: "/local/probe/c7", hash: []string{"id"}, partitions: 1,
			ttl: policy{Column: "ts", Tiers: []tier{{Seconds: 86400, Storage: "/local/probe/ext"}, {Seconds: 604800}}}},
		{name: "26-2-tiered-altered", path: "/local/probe/c4", hash: []string{"ts", "id"}, partitions: 64,
			ttl: policy{Column: "ts", Tiers: []tier{{Seconds: 86400, Storage: "/local/probe/ext"}}}},
		{name: "26-2-tiered-epoch", path: "/local/probe/c5", hash: []string{"ts"}, partitions: 64,
			ttl: policy{Column: "ts", Unit: 1, Tiers: []tier{{Seconds: 86400, Storage: "/local/probe/ext"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			body, described := captured(c, test.name)
			result, err := columnstore.Decode(body, test.path, described)
			c.Assert(err, qt.IsNil)
			c.Assert(result.HashColumns, qt.DeepEquals, test.hash)
			c.Assert(result.Partitions, qt.Equals, test.partitions)
			c.Assert(result.DataChannelCount, qt.Equals, uint64(64))
			c.Assert(result.LocalIndexes, qt.HasLen, 0)
			c.Assert(policyOf(result.Ttl), qt.DeepEquals, test.ttl)
		})
	}
}

// TestCapturedTTLDisagreementReturnsNoFacet requires the table service and the
// monitoring page to describe one TTL.
func TestCapturedTTLDisagreementReturnsNoFacet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.ColumnTableDescribeResult)
	}{
		{"no TTL", func(v *native.ColumnTableDescribeResult) { v.TtlSettings = nil }},
		{"other column", func(v *native.ColumnTableDescribeResult) {
			v.GetTtlSettings().GetTieredTtl().Tiers[0].GetDateTypeColumn().ColumnName = "id"
		}},
		{"other seconds", func(v *native.ColumnTableDescribeResult) {
			v.GetTtlSettings().GetTieredTtl().Tiers[1].GetDateTypeColumn().ExpireAfterSeconds = 1
		}},
		{"other storage", func(v *native.ColumnTableDescribeResult) {
			v.GetTtlSettings().GetTieredTtl().Tiers[0].GetEvictToExternalStorage().Storage = "/local/other"
		}},
		{"other action", func(v *native.ColumnTableDescribeResult) {
			v.GetTtlSettings().GetTieredTtl().Tiers[1].Action = &native.TtlTier_EvictToExternalStorage{EvictToExternalStorage: &native.EvictionToExternalStorageSettings{Storage: "/local/probe/ext"}}
		}},
		{"tier without action", func(v *native.ColumnTableDescribeResult) { v.GetTtlSettings().GetTieredTtl().Tiers[1].Action = nil }},
		{"tier without expression", func(v *native.ColumnTableDescribeResult) { v.GetTtlSettings().GetTieredTtl().Tiers[1].Expression = nil }},
		{"tier missing", func(v *native.ColumnTableDescribeResult) {
			v.GetTtlSettings().GetTieredTtl().Tiers = v.GetTtlSettings().GetTieredTtl().Tiers[:1]
		}},
		{"run interval", func(v *native.ColumnTableDescribeResult) { v.TtlSettings.RunIntervalSeconds = 3600 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			body, described := captured(c, "26-2-tiered-inline")
			test.change(described)
			result, err := columnstore.Decode(body, "/local/probe/c7", described)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// TestCapturedDeleteOnlyTTLAsAMode reads the table service's delete-only TTL,
// which it writes as a date mode, against the one tier the page reports.
func TestCapturedDeleteOnlyTTLAsAMode(t *testing.T) {
	c := qt.New(t)
	body, described := captured(c, "26-2-delete-ttl")
	c.Assert(described.GetTtlSettings().GetDateTypeColumn().GetExpireAfterSeconds(), qt.Equals, uint32(7200))
	described.GetTtlSettings().GetDateTypeColumn().ExpireAfterSeconds = 3600
	result, err := columnstore.Decode(body, "/local/probe/c3", described)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, `YDB column TTL disagrees across descriptions`)
}

func TestMonitoringRefusesUnknownIncompleteOrConflictingSettings(t *testing.T) {
	for _, test := range []struct{ name, old, replacement string }{
		{"unknown schema setting", `"Schema": {`, `"Schema": {"Unknown": true,`},
		{"wrong path", `"/local/col_index_probe"`, `"/local/other"`},
		{"wrong table name", `"Name": "col_index_probe"`, `"Name": "other"`},
		{"wrong status", `"StatusSuccess"`, `"StatusError"`},
		{"wrong column type", `"Type": "Utf8"`, `"Type": "String"`},
		{"wrong native type id", `"TypeId": 4608`, `"TypeId": 4`},
		{"wrong nullability", `"NotNull": false`, `"NotNull": true`},
		{"restore", `"IsRestore": false`, `"IsRestore": true`},
		{"actualization", `"SchemeNeedActualization": false`, `"SchemeNeedActualization": true`},
		{"no shards", `"ColumnShardCount": 64`, `"ColumnShardCount": 0`},
		{"unknown hash", `"Function": 2`, `"Function": 3`},
		{"the hash native CreateTable writes", `"Function": 2`, `"Function": 0`},
		{"unknown index column", `"ColumnId": 2`, `"ColumnId": 900`},
		{"configured column storage", `"StorageId": ""`, `"StorageId": "cold"`},
		{"unmodeled default", `"DefaultValue": {}`, `"DefaultValue": {"Value": 1}`},
		{"unmodeled type info", `"TypeId": 4`, `"TypeId": 4, "TypeInfo": {"Special": true}`},
		{"unknown index setting", `"BloomFilter": {`, `"BloomFilter": {"Unknown": true,`},
		{"wrong index class", `"ClassName": "BLOOM_FILTER"`, `"ClassName": "MIN_MAX"`},
		{"unknown encoding", `"ClassName": "DEFAULT"`, `"ClassName": "NEW"`},
		{"invalid probability", `"FalsePositiveProbability": 0.01`, `"FalsePositiveProbability": 1.0`},
		{"duplicate id", `"Id": 2`, `"Id": 1`},
		{"duplicate index id", `"Id": 8`, `"Id": 7`},
		{"duplicate index name", `"Name": "ng_default"`, `"Name": "bf_default"`},
		{"duplicate field", `"ColumnShardCount": 64`, `"ColumnShardCount": 64, "ColumnShardCount": 1`},
		{"duplicate alias", `"ColumnShardCount": 64`, `"ColumnShardCount": 64, "column_shard_count": 1`},
		{"wrong field case", `"ColumnShardCount": 64`, `"columnshardcount": 64`},
		{"duplicate path", `"Path": "/local/col_index_probe"`, `"Path": "/local/col_index_probe", "Path": "/local/col_index_probe"`},
		{"duplicate description", `"ColumnTableDescription": {`, `"ColumnTableDescription": {}, "ColumnTableDescription": {`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(strings.Contains(string(indexDescription), test.old), qt.IsTrue)
			body := strings.Replace(string(indexDescription), test.old, test.replacement, 1)
			result, err := columnstore.Decode([]byte(body), "/local/col_index_probe", indexTable())
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// TestMonitoringPageMayHoldBytesOutsideTheDescription reads a page whose access
// lists are raw bytes, as every live page is; the description itself has to be
// canonical JSON.
func TestMonitoringPageMayHoldBytesOutsideTheDescription(t *testing.T) {
	c := qt.New(t)
	body, described := captured(c, "26-2-hash")
	c.Assert(strings.ToValidUTF8(string(body), "") != string(body), qt.IsTrue, qt.Commentf("the capture holds no raw bytes"))
	result, err := columnstore.Decode(body, "/local/probe/c1", described)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Partitions, qt.Equals, uint64(3))
}

func TestNativeDisagreementReturnsNoFacet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.ColumnTableDescribeResult)
	}{
		{"row store", func(v *native.ColumnTableDescribeResult) { v.StoreType = Ydb_Table.StoreType_STORE_TYPE_ROW }},
		{"wrong identity", func(v *native.ColumnTableDescribeResult) { v.Self.Name = "other" }},
		{"wrong kind", func(v *native.ColumnTableDescribeResult) { v.Self.Type = Ydb_Scheme.Entry_TABLE }},
		{"different key", func(v *native.ColumnTableDescribeResult) { v.PrimaryKey = []string{"body"} }},
		{"different column order", func(v *native.ColumnTableDescribeResult) { v.Columns[0], v.Columns[1] = v.Columns[1], v.Columns[0] }},
		{"missing column", func(v *native.ColumnTableDescribeResult) { v.Columns = v.Columns[:1] }},
		{"nil column", func(v *native.ColumnTableDescribeResult) { v.Columns[0] = nil }},
		{"not null override", func(v *native.ColumnTableDescribeResult) { v.Columns[1].NotNull = proto.Bool(true) }},
		{"different hash", func(v *native.ColumnTableDescribeResult) {
			v.PartitioningSettings = &Ydb_Table.PartitioningSettings{PartitionBy: []string{"body"}}
		}},
		{"no hash", func(v *native.ColumnTableDescribeResult) { v.PartitioningSettings = nil }},
		{"partitioning beyond the hash", func(v *native.ColumnTableDescribeResult) { v.PartitioningSettings.MinPartitionsCount = 3 }},
		{"column family", func(v *native.ColumnTableDescribeResult) {
			v.ColumnFamilies = []*Ydb_Table.ColumnFamily{{Name: "default"}}
		}},
		{"column in a family", func(v *native.ColumnTableDescribeResult) { v.Columns[1].Family = "cold" }},
		{"column default", func(v *native.ColumnTableDescribeResult) {
			v.Columns[1].DefaultValue = &native.ColumnMeta_EmptyDefault{}
		}},
		{"global index", func(v *native.ColumnTableDescribeResult) {
			v.Indexes = []*native.TableIndexDescription{{Name: "by_body"}}
		}},
		{"attributes", func(v *native.ColumnTableDescribeResult) { v.Attributes = map[string]string{"k": "v"} }},
		{"key bloom filter", func(v *native.ColumnTableDescribeResult) { v.KeyBloomFilter = Ydb.FeatureFlag_ENABLED }},
		{"read replicas", func(v *native.ColumnTableDescribeResult) { v.ReadReplicasSettings = &Ydb_Table.ReadReplicasSettings{} }},
		{"storage settings", func(v *native.ColumnTableDescribeResult) { v.StorageSettings = &Ydb_Table.StorageSettings{} }},
		{"tiering", func(v *native.ColumnTableDescribeResult) { v.Tiering = "tier" }},
		{"temporary", func(v *native.ColumnTableDescribeResult) { v.Temporary = true }},
		{"TTL the page does not report", func(v *native.ColumnTableDescribeResult) {
			v.TtlSettings = &native.TtlSettings{Mode: &native.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "id", ExpireAfterSeconds: 60}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			source := indexTable()
			test.change(source)
			result, err := columnstore.Decode(indexDescription, "/local/col_index_probe", source)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}
