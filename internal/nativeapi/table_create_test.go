package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type creationFixture struct {
	Ydb_Scheme_V1.SchemeServiceClient
	events                                                     []string
	occupied, writeFailure, readFailure, changed, closeFailure bool
	attributeDrift                                             bool
}

func (f *creationFixture) ListDirectory(_ context.Context, _ *Ydb_Scheme.ListDirectoryRequest, _ ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	f.events = append(f.events, "list")
	listing := &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}}
	if f.occupied {
		listing.Children = []*Ydb_Scheme.Entry{{Name: "records", Type: Ydb_Scheme.Entry_TABLE}}
	}
	return &Ydb_Scheme.ListDirectoryResponse{Operation: result(listing)}, nil
}
func (f *creationFixture) Create(_ context.Context, request *Ydb_Table.CreateTableRequest) error {
	f.events = append(f.events, "create")
	if request.Path != "/local/app/records" {
		return errors.New("wrong dispatch path")
	}
	if f.writeFailure {
		return errors.New("lost native reply")
	}
	return nil
}
func (f *creationFixture) Describe(_ context.Context, _ string) (*native.DescribeTableResult, error) {
	f.events = append(f.events, "describe")
	if f.readFailure {
		return nil, errors.New("lost confirmation")
	}
	name := "id"
	if f.changed {
		name = "other"
	}
	attributes := map[string]string(nil)
	if f.attributeDrift {
		attributes = map[string]string{"unexpected": ""}
	}
	return &native.DescribeTableResult{Attributes: attributes, Self: &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE},
		PrimaryKey: []string{"id"}, Columns: []*native.ColumnMeta{{Name: name, Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}, NotNull: new(true)}}}, nil
}
func (f *creationFixture) Close() error {
	f.events = append(f.events, "close")
	if f.closeFailure {
		return errors.New("unconfirmed session release")
	}
	return nil
}
func TestNativeTableCreationDispatchAndConfirmation(t *testing.T) {
	for _, test := range []struct {
		name                             string
		fixture                          creationFixture
		guardFailure, dispatched, failed bool
		events                           []string
	}{
		{"confirmed", creationFixture{}, false, true, false, []string{"list", "open", "guard", "create", "describe", "close"}},
		{"occupied", creationFixture{occupied: true}, false, false, true, []string{"list"}},
		{"lease lost", creationFixture{}, true, false, true, []string{"list", "open", "guard", "close"}},
		{"lost reply", creationFixture{writeFailure: true}, false, true, true, []string{"list", "open", "guard", "create", "close"}},
		{"lost read", creationFixture{readFailure: true}, false, true, true, []string{"list", "open", "guard", "create", "describe", "close"}},
		{"changed result", creationFixture{changed: true}, false, true, true, []string{"list", "open", "guard", "create", "describe", "close"}},
		{"empty attribute changed name", creationFixture{attributeDrift: true}, false, true, true, []string{"list", "open", "guard", "create", "describe", "close"}},
		{"unconfirmed cleanup", creationFixture{closeFailure: true}, false, true, true, []string{"list", "open", "guard", "create", "describe", "close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := test.fixture
			api := &nativeapi.TableCreation{Scheme: &fixture, Open: func(context.Context) (nativeapi.CreationSession, error) {
				fixture.events = append(fixture.events, "open")
				return &fixture, nil
			}}
			owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
			operation, err := tablecreate.Capture("create", owner, &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"}, Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}}})
			c.Assert(err, qt.IsNil)
			if fixture.attributeDrift {
				request, err := tablecreate.Request(operation.Value)
				c.Assert(err, qt.IsNil)
				request.Attributes = map[string]string{"expected": ""}
				operation.Value.Payload, err = protojson.Marshal(request)
				c.Assert(err, qt.IsNil)
			}
			original := proto.CloneOf(operation.Value)
			dispatched, err := api.Apply(t.Context(), "/local", operation.Value, func(context.Context) error {
				fixture.events = append(fixture.events, "guard")
				if test.guardFailure {
					return errors.New("lease lost")
				}
				return nil
			})
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(err != nil, qt.Equals, test.failed)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
			c.Assert(proto.Equal(original, operation.Value), qt.IsTrue)
		})
	}
}
