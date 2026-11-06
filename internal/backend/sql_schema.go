package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
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
	if err := provider.ValidateReadSQLSchemaRequest(described(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	reply := yqlschema.Reply(request)
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateSQLSchema(described(), request, reply); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return reply, nil
}

var _ provider.SQLSchemaReader = (*Provider)(nil)
