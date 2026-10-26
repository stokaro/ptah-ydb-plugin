package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ResolveDatabaseContext interprets the address without opening a connection
// or resolving credentials. A dev realm retains the authored database root.
func (*Provider) ResolveDatabaseContext(ctx context.Context, request *protocol.DatabaseContextRequest) (*protocol.DatabaseContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateDatabaseContextRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	address, err := connection.Parse(request.GetAddress())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := databaseContext(address)
	if err := schema.ValidateDatabaseContext(result); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

func databaseContext(address connection.Address) *protocol.DatabaseContext {
	return &protocol.DatabaseContext{Database: address.Root(), SourceDatabase: address.Database()}
}

func bindInspectionContext(snapshot *protocol.Schema, bound *protocol.DatabaseContext) {
	bind := func(values []*protocol.Envelope) {
		for _, value := range values {
			value.Context = proto.CloneOf(bound)
		}
	}
	bind(snapshot.NativeObjects)
	for _, table := range snapshot.Tables {
		bind(table.Facets)
		for _, column := range table.Columns {
			bind(column.Facets)
		}
	}
	for _, view := range snapshot.GetCommonObjects().GetViews() {
		bind(view.Facets)
	}
}

var _ provider.DatabaseContextResolver = (*Provider)(nil)
