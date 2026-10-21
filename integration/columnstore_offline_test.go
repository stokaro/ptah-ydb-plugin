//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

func TestColumnStorageOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	body, err := os.ReadFile("../internal/columnstore/testdata/26-2-indexes.json")
	c.Assert(err, qt.IsNil)
	observed, err := columnstore.Decode(body, "/local/col_index_probe", &native.DescribeTableResult{
		Self:      &Ydb_Scheme.Entry{Name: "col_index_probe", Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		StoreType: Ydb_Table.StoreType_STORE_TYPE_COLUMN, PrimaryKey: []string{"id"},
		Columns: []*native.ColumnMeta{
			{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}},
			{Name: "body", Type: &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}}},
		},
	})
	c.Assert(err, qt.IsNil)
	envelope, err := schema.CaptureColumnStorage(&protocol.Reference{Engine: "ydb", Database: "/local", Kind: "table", Name: "col_index_probe"}, observed)
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "column-storage", Representation: protocol.ValueInput_OBSERVED, Value: envelope},
	}}
	normalized, err := client.NormalizeValues(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedValues(client.Description(), request, normalized), qt.IsNil)
	again, err := client.NormalizeValues(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, again), qt.IsTrue)
	bad := proto.CloneOf(request)
	bad.Values[0].Value.Payload = []byte(`{"hashColumns":["id"],"partitions":"1","dataChannelCount":"64","localIndexes":[{"name":"missing-kind","columns":["id"]}]}`)
	_, err = client.NormalizeValues(ctx, bad)
	c.Assert(err, qt.IsNotNil)
}
