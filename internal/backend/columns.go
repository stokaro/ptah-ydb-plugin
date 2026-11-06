package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
)

// CompareColumns interprets exactly the host's paired columns and requested
// aspects. The complete parent captures keep key membership and storage intent.
func (*Provider) CompareColumns(ctx context.Context, request *protocol.CompareColumnsRequest) (*protocol.ComparedColumns, error) {
	description, err := columnsContext(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := provider.ValidateColumnsRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := columnsemantics.Compare(ctx, request)
	if err != nil {
		return nil, columnsError(ctx, err)
	}
	if err := provider.ValidateComparedColumnsReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
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
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := columnsemantics.ReadDefaults(ctx, request)
	if err != nil {
		return nil, columnsError(ctx, err)
	}
	if err := provider.ValidateColumnDefaultsReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
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
	if !rulesMatch(request.GetIdentifiers().GetRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB column semantics require its negotiated identifier rules")
	}
	return provider.NegotiatedDescription(Description(), request.GetTargetFeatures()), nil
}

func columnsError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return authored.Status(codes.InvalidArgument, err)
}
