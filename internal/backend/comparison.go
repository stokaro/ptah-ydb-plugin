package backend

import (
	"context"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingcompare"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadcompare"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CompareModels compares captured coordination schemas and row-table facets
// without session access. Coverage and complete operands determine absence,
// equality, and safety facts.
func (*Provider) CompareModels(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidateComparisonRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !proto.Equal(request.GetIdentifiers().GetRules(), identifierRules()) {
		return nil, status.Error(codes.InvalidArgument, "YDB comparison requires its negotiated identifier rules")
	}
	compare := coordinationcompare.Compare
	switch request.GetComparisonId() {
	case secretcompare.ServiceID:
		compare = secretcompare.Compare
	case tablecompare.ServiceID:
		compare = tablecompare.Compare
	case streamingcompare.ServiceID:
		compare = streamingcompare.Compare
	case topiccompare.ServiceID:
		compare = topiccompare.Compare
	case externalcompare.ServiceID:
		compare = externalcompare.Compare
	case workloadcompare.PoolServiceID:
		compare = workloadcompare.ComparePools
	case workloadcompare.ClassifierServiceID:
		compare = workloadcompare.CompareClassifiers
	}
	result, err := compare(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := provider.ValidateComparedModels(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
