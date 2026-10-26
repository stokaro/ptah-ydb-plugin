package backend

import (
	"context"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
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
	// Optional common kinds belong to the negotiated request profile. A host
	// without independent access observations cannot supply their coverage.
	description := provider.NegotiatedDescription(Description(), request.GetTargetFeatures())
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
	case streamingplan.ServiceID:
		result, err = streamingplan.Plan(ctx, request)
	case topicplan.ServiceID:
		result, err = topicplan.Plan(ctx, request)
	case externalplan.ServiceID:
		result, err = externalplan.Plan(ctx, request)
	case tablecreate.ServiceID:
		var stripped *protocol.PlanFeaturesRequest
		if stripped, err = planningWithoutComments(request); err == nil {
			result, err = tablecreate.Plan(ctx, stripped)
		}
	case workloadplan.ServiceID:
		result, err = workloadplan.Plan(ctx, request)
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
// YQL statement of each; see renderText. A view or a comment on the common
// wire is YQL in either form: Ptah's COMMENT ON statement is what execution
// reads. Both forms check the whole batch before writing a unit.
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
	// Each change is a native operation or, for a common object, its
	// statement.
	operations := make([]*protocol.Envelope, len(request.Changes))
	statements := make([]string, len(request.Changes))
	for i, change := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if common := change.GetCommonObject(); common != nil {
			statement, err := commonobject.Render(common, request.GetCapabilities())
			if err != nil {
				return nil, status.Error(codes.FailedPrecondition, err.Error())
			}
			statements[i] = statement
			continue
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
		if captured.GetValue().GetKind() == schema.ChangefeedOperation && !request.Capabilities[changefeeds.CapabilityChangefeeds] {
			return nil, status.Error(codes.FailedPrecondition, "changefeed rendering requires the changefeeds capability")
		}
		if kind := captured.GetValue().GetKind(); (kind == schema.ExternalSourceOperation || kind == schema.ExternalTableOperation) && !request.Capabilities[externals.CapabilityExternalDataSources] {
			return nil, status.Error(codes.FailedPrecondition, "external object rendering requires the external_data_sources capability")
		}
		if captured.GetValue().GetKind() == schema.StreamingQueryOperation && !request.Capabilities[streaming.Capability] {
			return nil, status.Error(codes.FailedPrecondition, streaming.CapabilityRefusal("streaming query"))
		}
		if captured.GetValue().GetKind() == schema.WorkloadOperation && !request.Capabilities[workload.CapabilityResourcePools] {
			return nil, status.Error(codes.FailedPrecondition, "resource pools require target capability resource_pools; "+workload.FlagHint)
		}
		normalized, err := normalizeOperation(&protocol.ValueInput{Id: captured.GetId(), Representation: protocol.ValueInput_OPERATION, Value: captured.GetValue()})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !proto.Equal(normalized, captured) {
			return nil, status.Error(codes.InvalidArgument, "native operation metadata changed")
		}
		operations[i] = normalized.Value
	}
	result := &protocol.RenderResult{Complete: true}
	if request.GetForm() == protocol.RenderRequest_TEXT {
		text, err := renderText(ctx, request.Changes, operations, statements)
		if err != nil {
			return nil, err
		}
		result = text
	} else {
		for i, change := range request.Changes {
			unit := &protocol.Unit{ChangeId: change.Id, Body: &protocol.Unit_Sql{Sql: statements[i]}}
			if operations[i] != nil {
				unit.Body = &protocol.Unit_Native{Native: proto.CloneOf(operations[i])}
			}
			result.Units = append(result.Units, unit)
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
