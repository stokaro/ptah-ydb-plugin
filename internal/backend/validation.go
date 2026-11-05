package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/schemavalidation"
)

// ValidateSchema checks the captured desired schema offline. A refusal of the
// declaration is a typed diagnostic in a completed reply; an unusable request,
// cancellation or a reply the SDK would not accept returns an error and no
// diagnostics.
func (*Provider) ValidateSchema(ctx context.Context, request *protocol.ValidateSchemaRequest) (*protocol.SchemaValidation, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidateSchemaRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if !proto.Equal(request.Identifiers.GetRules(), identifierRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB validation requires its negotiated identifier rules")
	}
	result, err := schemavalidation.Validate(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := provider.ValidateSchemaResult(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}
