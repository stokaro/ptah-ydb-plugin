package backend

import (
	"context"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
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
	var result *protocol.PlannedFeatures
	var err error
	switch request.PlanningId {
	case coordinationplan.ServiceID:
		result, err = coordinationplan.Plan(ctx, request)
	case topicplan.ServiceID:
		result, err = topicplan.Plan(ctx, request)
	case tablecreate.ServiceID:
		result, err = tablecreate.Plan(ctx, request)
	default:
		err = fmt.Errorf("unsupported YDB planning service")
	}
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

// Render validates complete captured operations and emits native envelopes,
// or, in the TEXT form a host asks for where it writes a migration file, the
// YQL statement of each; see renderText. Both forms check the whole batch
// before writing a unit.
func (*Provider) Render(ctx context.Context, request *protocol.RenderRequest) (*protocol.RenderResult, error) {
	if err := provider.ValidateRenderRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(request.Changes) > 0 {
		transactional, known := request.Capabilities["transactional_ddl"]
		if !known || transactional {
			return nil, status.Error(codes.FailedPrecondition, "native YDB rendering requires established nontransactional DDL")
		}
	}
	operations := make([]*protocol.Envelope, 0, len(request.Changes))
	for _, change := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		native := change.GetNative()
		if native.GetRole() != protocol.NativeOperation_STATEMENT || native.GetParent() != nil {
			return nil, status.Error(codes.InvalidArgument, "only standalone native operations are implemented")
		}
		captured := native.GetValue()
		if captured.GetValue().GetKind() == schema.CoordinationOperation && !request.Capabilities["coordination_nodes"] {
			return nil, status.Error(codes.FailedPrecondition, "native coordination rendering requires established capabilities")
		}
		if captured.GetValue().GetKind() == schema.TopicOperation && !request.Capabilities[topics.CapabilityTopics] {
			return nil, status.Error(codes.FailedPrecondition, "topic rendering requires the topics capability")
		}
		normalized, err := normalizeOperation(&protocol.ValueInput{Id: captured.GetId(), Representation: protocol.ValueInput_OPERATION, Value: captured.GetValue()})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !proto.Equal(normalized, captured) {
			return nil, status.Error(codes.InvalidArgument, "native operation metadata changed")
		}
		operations = append(operations, normalized.Value)
	}
	result := &protocol.RenderResult{Complete: true}
	if request.GetForm() == protocol.RenderRequest_TEXT {
		text, err := renderText(ctx, request.Changes, operations)
		if err != nil {
			return nil, err
		}
		result = text
	} else {
		for i, change := range request.Changes {
			result.Units = append(result.Units, &protocol.Unit{ChangeId: change.Id, Body: &protocol.Unit_Native{Native: proto.CloneOf(operations[i])}})
		}
	}
	if err := provider.ValidateRenderedBatch(Description(), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
