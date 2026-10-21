package metadata_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

func TestMetadataOwnershipUsesCompleteFormat(t *testing.T) {
	for _, kind := range []protocol.MetadataScope_Kind{protocol.MetadataScope_LOG, protocol.MetadataScope_TAGS} {
		t.Run(kind.String(), func(t *testing.T) {
			c := qt.New(t)
			store, err := metadata.Resolve("/local", &protocol.MetadataScope{SessionId: "s", Schema: "app", Table: "store", Kind: kind})
			c.Assert(err, qt.IsNil)
			request := store.CreateRequest()
			original := &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "store", Type: Ydb_Scheme.Entry_TABLE}, StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW, PrimaryKey: request.PrimaryKey, Attributes: request.Attributes}
			for _, column := range request.Columns {
				original.Columns = append(original.Columns, &native.ColumnMeta{Name: column.Name, Type: column.Type, NotNull: column.NotNull})
			}
			for _, test := range []struct {
				name          string
				mutate        func(*native.DescribeTableResult)
				owned, failed bool
			}{
				{"owned", func(*native.DescribeTableResult) {}, true, false},
				{"unmarked", func(v *native.DescribeTableResult) { v.Attributes = nil }, false, false},
				{"unknown marker", func(v *native.DescribeTableResult) { v.Attributes["ptah.metadata"] = "unknown" }, false, true},
				{"wrong key", func(v *native.DescribeTableResult) { v.PrimaryKey = []string{"record"} }, false, true},
				{"extra attributes", func(v *native.DescribeTableResult) { v.Attributes["foreign"] = "value" }, false, true},
				{"ttl", func(v *native.DescribeTableResult) { v.TtlSettings = &Ydb_Table.TtlSettings{} }, false, true},
				{"index", func(v *native.DescribeTableResult) { v.Indexes = []*native.TableIndexDescription{{Name: "extra"}} }, false, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					c := qt.New(t)
					value := proto.CloneOf(original)
					test.mutate(value)
					purpose, err := metadata.Owned("store", value)
					if test.failed {
						c.Assert(err, qt.IsNotNil)
						return
					}
					c.Assert(err, qt.IsNil)
					if !test.owned {
						c.Assert(purpose, qt.Equals, protocol.ReservedObject_PURPOSE_UNSPECIFIED)
					} else if kind == protocol.MetadataScope_LOG {
						c.Assert(purpose, qt.Equals, protocol.ReservedObject_LOG)
					} else {
						c.Assert(purpose, qt.Equals, protocol.ReservedObject_TAG)
					}
				})
			}
		})
	}
}
