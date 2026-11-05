package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// withUnknown is a request family named hot whose unknown fields are raw.
func withUnknown(raw []byte) *Ydb_Table.ColumnFamily {
	family := &Ydb_Table.ColumnFamily{Name: "hot"}
	family.ProtoReflect().SetUnknown(raw)
	return family
}

// varint is one varint field with its tag.
func varint(number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, number, protowire.VarintType), value)
}

// A request's family carries the cache mode as the wire bytes the upstream
// ColumnFamily has, so the table service reads what the provider's family
// states, and reading the request's family back gives the same family.
func TestRequestFamily_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		family *native.ColumnFamily
	}{
		{name: "in memory", family: &native.ColumnFamily{Name: "hot", CacheMode: native.ColumnFamily_CACHE_MODE_IN_MEMORY}},
		{name: "regular beside every other setting", family: &native.ColumnFamily{Name: "cold", Data: &Ydb_Table.StoragePool{Media: "hdd"},
			Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4, KeepInMemory: Ydb.FeatureFlag_DISABLED, CacheMode: native.ColumnFamily_CACHE_MODE_REGULAR}},
		{name: "no cache mode", family: &native.ColumnFamily{Name: "default", Compression: Ydb_Table.ColumnFamily_COMPRESSION_NONE}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := schema.RequestFamily(test.family)
			sent, err := proto.Marshal(request)
			c.Assert(err, qt.IsNil)
			upstream, err := proto.Marshal(test.family)
			c.Assert(err, qt.IsNil)
			c.Assert(sent, qt.DeepEquals, upstream)
			read, err := schema.NativeFamily(request)
			c.Assert(err, qt.IsNil)
			c.Assert(read, qt.CmpEquals(protocmp.Transform()), test.family)
			c.Assert(schema.Known(request), qt.IsNil)
		})
	}
}

// A request's family whose unknown fields hold anything but one cache mode
// the provider's family names is refused, by the reading and by Known.
func TestRequestFamily_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "a compression level", raw: varint(5, 3)},
		{name: "a cache mode the provider does not name", raw: varint(6, 3)},
		{name: "an unspecified cache mode written out", raw: varint(6, 0)},
		{name: "a cache mode past the enum's range", raw: varint(6, 1<<32+2)},
		{name: "two cache modes", raw: append(varint(6, 1), varint(6, 2)...)},
		{name: "a cache mode beside another field", raw: append(varint(6, 1), varint(7, 1)...)},
		{name: "a cache mode of another wire type", raw: protowire.AppendBytes(protowire.AppendTag(nil, 6, protowire.BytesType), []byte{2})},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			family := withUnknown(test.raw)
			read, err := schema.NativeFamily(family)
			c.Assert(err, qt.ErrorMatches, `unknown fields in Ydb.Table.ColumnFamily`)
			c.Assert(read, qt.IsNil)
			c.Assert(schema.Known(family), qt.ErrorMatches, `unknown fields in Ydb.Table.ColumnFamily`)
			c.Assert(schema.Known(&Ydb_Table.CreateTableRequest{ColumnFamilies: []*Ydb_Table.ColumnFamily{family}}), qt.ErrorMatches, `unknown fields in Ydb.Table.ColumnFamily`)
		})
	}
}

// Known takes the cache mode of a request's family wherever the family sits
// in a request, and the same field on any other message stays unknown.
func TestKnown_TakesTheCacheModeOfARequestFamilyOnly(t *testing.T) {
	c := testcheck.New(t)
	family := schema.RequestFamily(&native.ColumnFamily{Name: "hot", CacheMode: native.ColumnFamily_CACHE_MODE_IN_MEMORY})
	c.Assert(schema.Known(&Ydb_Table.CreateTableRequest{ColumnFamilies: []*Ydb_Table.ColumnFamily{family}}), qt.IsNil)
	c.Assert(schema.Known(&Ydb_Table.AlterTableRequest{AddColumnFamilies: []*Ydb_Table.ColumnFamily{family}, AlterColumnFamilies: []*Ydb_Table.ColumnFamily{family}}), qt.IsNil)
	pool := &Ydb_Table.StoragePool{Media: "hdd"}
	pool.ProtoReflect().SetUnknown(varint(6, 2))
	c.Assert(schema.Known(pool), qt.ErrorMatches, `unknown fields in Ydb.Table.StoragePool`)
}
