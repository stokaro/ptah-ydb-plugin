package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PlanFeatures contributes native operations to the host's complete shared graph.
func (*Provider) PlanFeatures(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidatePlanningRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !proto.Equal(request.Identifiers.GetRules(), identifierRules()) {
		return nil, status.Error(codes.InvalidArgument, "YDB planning requires its negotiated identifier rules")
	}
	result, err := coordinationplan.Plan(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := provider.ValidatePlannedFeatures(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

// Render validates complete captured operations and emits native envelopes.
// No synthetic coordination SQL is generated or sent to the query service.
func (*Provider) Render(ctx context.Context, request *protocol.RenderRequest) (*protocol.RenderResult, error) {
	if err := provider.ValidateRenderRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(request.Changes) > 0 {
		transactional, known := request.Capabilities["transactional_ddl"]
		if !known || transactional || !request.Capabilities["coordination_nodes"] {
			return nil, status.Error(codes.FailedPrecondition, "native coordination rendering requires established capabilities")
		}
	}
	result := &protocol.RenderResult{Complete: true}
	for _, change := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		native := change.GetNative()
		if native.GetRole() != protocol.NativeOperation_STATEMENT || native.GetParent() != nil {
			return nil, status.Error(codes.InvalidArgument, "only standalone coordination operations are implemented")
		}
		captured := native.GetValue()
		normalized, err := schema.NormalizeCoordinationOperation(&protocol.ValueInput{Id: captured.GetId(), Representation: protocol.ValueInput_OPERATION, Value: captured.GetValue()})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !proto.Equal(normalized, captured) {
			return nil, status.Error(codes.InvalidArgument, "coordination operation metadata changed")
		}
		result.Units = append(result.Units, &protocol.Unit{ChangeId: change.Id, Body: &protocol.Unit_Native{Native: proto.CloneOf(normalized.Value)}})
	}
	if err := provider.ValidateRenderedBatch(Description(), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
