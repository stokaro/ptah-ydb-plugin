package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
)

// ProjectTableCreations resolves CREATE-dependent intent from captured tables.
// It predicts the ordered key without inventing native defaults or observations.
func (*Provider) ProjectTableCreations(ctx context.Context, request *protocol.ProjectTableCreationsRequest) (*protocol.ProjectedTableCreations, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := provider.NegotiatedDescription(described(), request.GetTargetFeatures())
	if err := provider.ValidateCreationRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if !rulesMatch(request.Identifiers.GetRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB creation prediction requires its negotiated identifier rules")
	}
	stripped, err := creationsWithoutComments(request)
	if err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := tablecreate.ProjectCreations(ctx, stripped)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	// Common comments are rendered separately, but remain part of the effective
	// column definition that source projection returns.
	for i, table := range result.Tables {
		for j, column := range table.Columns {
			column.Comment = request.Tables[i].Declaration.Columns[j].Comment
		}
	}
	if err := provider.ValidateProjectedCreationsReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}
