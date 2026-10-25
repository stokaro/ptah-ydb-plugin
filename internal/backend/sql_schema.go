package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sqlSchemaDeclaration says that the ydb engine reads its SQL schema
// documents whole, in YQL, and which header kinds leave a YDB object
// undescribed.
func sqlSchemaDeclaration() *protocol.SQLSchemaDeclaration {
	return &protocol.SQLSchemaDeclaration{Mode: protocol.SQLSchemaDeclaration_DOCUMENT, HeaderKinds: yqlschema.HeaderKinds()}
}

// ReadSQLSchema reads the YQL schema documents of one load, in order, as the
// Ptah host's YDB owner reads them. It opens nothing and needs no session. A
// refused load is answered with complete false and diagnostics, never with a
// partial schema.
func (*Provider) ReadSQLSchema(ctx context.Context, request *protocol.ReadSQLSchemaRequest) (*protocol.SQLSchema, error) {
	if err := provider.ValidateReadSQLSchemaRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	reply := yqlschema.Reply(request)
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateSQLSchema(Description(), request, reply); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return reply, nil
}

var _ provider.SQLSchemaReader = (*Provider)(nil)
