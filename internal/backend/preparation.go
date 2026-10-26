package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PrepareTables derives native row-table key membership from captured input.
// The host retains source declarations, observations, and existence evidence.
func (*Provider) PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidatePreparationRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !proto.Equal(request.Identifiers.GetRules(), identifierRules()) {
		return nil, status.Error(codes.InvalidArgument, "YDB preparation requires its negotiated identifier rules")
	}
	stripped, err := preparationWithoutComments(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := tablecreate.PrepareTables(ctx, stripped)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := provider.ValidatePreparedTables(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}
