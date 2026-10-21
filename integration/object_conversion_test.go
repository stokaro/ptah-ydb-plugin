//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

func TestNativeObjectConversionOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	self := &Ydb_Scheme.Entry{Owner: "alice", Permissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"read"}}}}
	request := &protocol.NormalizeValuesRequest{Engine: "ydb"}
	for _, test := range []struct {
		kind  string
		value proto.Message
	}{
		{schema.ViewProperties, &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: proto.CloneOf(self), QueryText: "SELECT 1"}, Properties: &native.DescribeTableResult{Self: proto.CloneOf(self), Attributes: map[string]string{"comment": "view"}}}},
		{schema.ExternalSourceProperties, &Ydb_Table.DescribeExternalDataSourceResult{Self: proto.CloneOf(self), SourceType: proto.String("ObjectStorage"), Location: proto.String("https://storage.invalid/"), Properties: map[string]string{"AUTH_METHOD": "NONE"}}},
		{schema.ExternalTableProperties, &native.DescribeExternalTableResult{Self: proto.CloneOf(self), SourceType: proto.String("ObjectStorage"), DataSourcePath: proto.String("/local/App/bucket"), Location: proto.String("f/"), Columns: []*native.ColumnMeta{{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}}}, Content: map[string]string{"FORMAT": `["json_each_row"]`}}},
		{schema.CoordinationProperties, &Ydb_Coordination.DescribeNodeResult{Self: proto.CloneOf(self), Config: &Ydb_Coordination.Config{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 9000}}},
	} {
		owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "App", Kind: test.kind, Name: "Object"}
		value, err := schema.CaptureObject(owner, test.value)
		c.Assert(err, qt.IsNil)
		request.Values = append(request.Values, &protocol.ValueInput{Id: test.kind, Representation: protocol.ValueInput_OBSERVED, Value: value})
	}
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	convertNativeProperties(c, client, request)
}
