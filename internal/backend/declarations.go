package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PlanDeclarations creates authored native objects without an observed catalog.
func (p *Provider) PlanDeclarations(ctx context.Context, request *protocol.PlanDeclarationsRequest) (*protocol.PlannedDeclarations, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := provider.NegotiatedDescription(Description(), request.GetTargetFeatures())
	if err := provider.ValidateDeclarationsRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !proto.Equal(request.Identifiers.GetRules(), identifierRules()) || request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return nil, status.Error(codes.InvalidArgument, "YDB authored declarations require exact identifier rules")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return nil, status.Error(codes.InvalidArgument, "YDB authored declarations require exact resolved names")
		}
	}
	inputs := &protocol.NormalizeValuesRequest{Engine: request.Engine, TargetFeatures: request.TargetFeatures}
	for _, object := range request.Objects {
		inputs.Values = append(inputs.Values, &protocol.ValueInput{Id: object.Value.Id, Value: object.Value.Value, Representation: protocol.ValueInput_DESIRED})
	}
	normalized, err := p.NormalizeValues(ctx, inputs)
	if err != nil {
		return nil, err
	}
	for i, value := range normalized.Values {
		if !proto.Equal(value, request.Objects[i].Value) {
			return nil, status.Error(codes.InvalidArgument, "authored declaration normalization changed")
		}
	}
	result, err := declarationplan.Plan(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := provider.ValidatePlannedDeclarations(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
