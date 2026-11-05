package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PrepareTables derives native row-table key membership from captured input.
// The host retains source declarations, observations, and existence evidence.
func (*Provider) PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidatePreparationRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if !rulesMatch(request.Identifiers.GetRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB preparation requires its negotiated identifier rules")
	}
	stripped, err := preparationWithoutComments(request)
	if err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := tablecreate.PrepareTables(ctx, stripped)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := provider.ValidatePreparedTables(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}
