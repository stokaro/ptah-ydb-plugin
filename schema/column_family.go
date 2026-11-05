package schema

import (
	"fmt"
	"math"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// familyCacheModeField is the wire number of a column family's cache mode in
// the upstream table protocol. The pinned Go bindings do not declare it, and
// keep it as an unknown field of Ydb.Table.ColumnFamily.
const familyCacheModeField protowire.Number = 6

// RequestFamily returns family as a table service request takes it: the
// pinned Ydb.Table.ColumnFamily, with a cache mode in field 6, which the
// bindings keep as an unknown field and send with the request. A table
// service that does not know the field drops it without an error (measured on
// 25.1.4.7), so a caller sends a cache mode only to a target that takes one.
func RequestFamily(family *native.ColumnFamily) *Ydb_Table.ColumnFamily {
	result := &Ydb_Table.ColumnFamily{Name: family.GetName(), Data: proto.CloneOf(family.GetData()), Compression: family.GetCompression(),
		KeepInMemory: family.GetKeepInMemory()}
	if mode := family.GetCacheMode(); mode != native.ColumnFamily_CACHE_MODE_UNSPECIFIED {
		raw := protowire.AppendTag(nil, familyCacheModeField, protowire.VarintType)
		result.ProtoReflect().SetUnknown(protowire.AppendVarint(raw, uint64(mode)))
	}
	return result
}

// NativeFamily returns a request's family as the provider's ColumnFamily,
// with the cache mode the family carries in field 6. A family whose unknown
// fields hold anything else is refused.
func NativeFamily(family *Ydb_Table.ColumnFamily) (*native.ColumnFamily, error) {
	mode, read := requestCacheMode(family.ProtoReflect().GetUnknown())
	if !read {
		return nil, fmt.Errorf("unknown fields in %s", family.ProtoReflect().Descriptor().FullName())
	}
	return &native.ColumnFamily{Name: family.GetName(), Data: proto.CloneOf(family.GetData()), Compression: family.GetCompression(),
		KeepInMemory: family.GetKeepInMemory(), CacheMode: mode}, nil
}

// requestCacheMode reads the cache mode out of the unknown fields of a
// request's family: none for no unknown field, and one field 6 holding a mode
// the provider's ColumnFamily names. It reports false for anything else.
func requestCacheMode(unknown []byte) (native.ColumnFamily_CacheMode, bool) {
	if len(unknown) == 0 {
		return native.ColumnFamily_CACHE_MODE_UNSPECIFIED, true
	}
	number, wireType, length := protowire.ConsumeTag(unknown)
	if length < 0 || number != familyCacheModeField || wireType != protowire.VarintType {
		return native.ColumnFamily_CACHE_MODE_UNSPECIFIED, false
	}
	value, valueLength := protowire.ConsumeVarint(unknown[length:])
	if valueLength < 0 || length+valueLength != len(unknown) || value == 0 || value > math.MaxInt32 {
		return native.ColumnFamily_CACHE_MODE_UNSPECIFIED, false
	}
	if _, named := native.ColumnFamily_CacheMode_name[int32(value)]; !named {
		return native.ColumnFamily_CACHE_MODE_UNSPECIFIED, false
	}
	return native.ColumnFamily_CacheMode(value), true
}
