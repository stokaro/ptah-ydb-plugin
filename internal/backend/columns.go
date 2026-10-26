package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CompareColumns interprets exactly the host's paired columns and requested
// aspects. The complete parent captures keep key membership and storage intent.
func (*Provider) CompareColumns(ctx context.Context, request *protocol.CompareColumnsRequest) (*protocol.ComparedColumns, error) {
	description, err := columnsContext(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := provider.ValidateColumnsRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := columnsemantics.Compare(ctx, request)
	if err != nil {
		return nil, columnsError(ctx, err)
	}
	if err := provider.ValidateComparedColumns(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// ReadColumnDefaults decodes the typed literals YDB reports without treating
// an empty literal as an absent default or interpreting arbitrary expressions.
func (*Provider) ReadColumnDefaults(ctx context.Context, request *protocol.ReadColumnDefaultsRequest) (*protocol.ReadColumnDefaultsResult, error) {
	description, err := columnsContext(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := provider.ValidateColumnDefaultsRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := columnsemantics.ReadDefaults(ctx, request)
	if err != nil {
		return nil, columnsError(ctx, err)
	}
	if err := provider.ValidateColumnDefaults(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

type columnsRequest interface {
	GetTargetFeatures() []string
	GetIdentifiers() *protocol.ComparisonIdentifiers
}

func columnsContext(ctx context.Context, request columnsRequest) (*protocol.Description, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if !proto.Equal(request.GetIdentifiers().GetRules(), identifierRules()) {
		return nil, status.Error(codes.InvalidArgument, "YDB column semantics require its negotiated identifier rules")
	}
	return provider.NegotiatedDescription(Description(), request.GetTargetFeatures()), nil
}

func columnsError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
