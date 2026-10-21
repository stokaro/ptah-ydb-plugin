package columnstore_test

import (
	_ "embed"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
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

func table(name string, columns ...*native.ColumnMeta) *native.DescribeTableResult {
	return &native.DescribeTableResult{
		Self:       &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		StoreType:  Ydb_Table.StoreType_STORE_TYPE_COLUMN,
		PrimaryKey: []string{"id"}, Columns: columns,
	}
}

func indexTable() *native.DescribeTableResult {
	return table("col_index_probe", column("id", Ydb.Type_UINT64, false), column("body", Ydb.Type_UTF8, true))
}

func TestCapturedDescriptionsPreserveStorage(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       []byte
		table      *native.DescribeTableResult
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
		unit          uint64
		hasExpiration bool
	}{
		{"col_tier_probe", ttlDescription, Ydb.Type_TIMESTAMP, 0, true},
		{"col_epoch_probe", epochDescription, Ydb.Type_UINT64, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			described := table(test.name, column("at", test.typ, false), column("id", Ydb.Type_UINT64, false))
			described.PrimaryKey = []string{"at", "id"}
			result, err := columnstore.Decode(test.body, "/local/"+test.name, described)
			c.Assert(err, qt.IsNil)
			ttl := result.Ttl.GetEnabled()
			c.Assert(ttl.Column, qt.Equals, "at")
			c.Assert(ttl.ColumnUnit, qt.Equals, test.unit)
			c.Assert(ttl.ExpireAfterSeconds != nil, qt.Equals, test.hasExpiration)
			c.Assert(ttl.Tiers, qt.HasLen, 2)
			c.Assert(ttl.Tiers[0].ApplyAfterSeconds, qt.Equals, uint64(86400))
			c.Assert(ttl.Tiers[0].GetExternalStorage(), qt.Equals, "/local/col_archive")
			c.Assert(ttl.Tiers[1].ApplyAfterSeconds, qt.Equals, uint64(604800))
			c.Assert(ttl.Tiers[1].GetDelete(), qt.IsNotNil)
		})
	}
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

func TestNativeDisagreementReturnsNoFacet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.DescribeTableResult)
	}{
		{"row store", func(v *native.DescribeTableResult) { v.StoreType = Ydb_Table.StoreType_STORE_TYPE_ROW }},
		{"wrong identity", func(v *native.DescribeTableResult) { v.Self.Name = "other" }},
		{"wrong kind", func(v *native.DescribeTableResult) { v.Self.Type = Ydb_Scheme.Entry_TABLE }},
		{"different key", func(v *native.DescribeTableResult) { v.PrimaryKey = []string{"body"} }},
		{"different column order", func(v *native.DescribeTableResult) { v.Columns[0], v.Columns[1] = v.Columns[1], v.Columns[0] }},
		{"missing column", func(v *native.DescribeTableResult) { v.Columns = v.Columns[:1] }},
		{"nil column", func(v *native.DescribeTableResult) { v.Columns[0] = nil }},
		{"not null override", func(v *native.DescribeTableResult) { v.Columns[1].NotNull = proto.Bool(true) }},
		{"different hash", func(v *native.DescribeTableResult) {
			v.PartitioningSettings = &Ydb_Table.PartitioningSettings{PartitionBy: []string{"body"}}
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
