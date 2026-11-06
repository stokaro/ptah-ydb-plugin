package backend

import (
	"context"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// PlanFeatures contributes native operations to the host's complete shared graph.
func (*Provider) PlanFeatures(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	// Optional common kinds belong to the negotiated request profile. A host
	// without independent access observations cannot supply their coverage.
	description := provider.NegotiatedDescription(described(), request.GetTargetFeatures())
	if err := provider.ValidatePlanningRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if !rulesMatch(request.Identifiers.GetRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB planning requires its negotiated identifier rules")
	}
	var result *protocol.PlannedFeatures
	var err error
	switch request.PlanningId {
	case secretplan.ServiceID:
		result, err = secretplan.Plan(ctx, request)
	case coordinationplan.ServiceID:
		result, err = coordinationplan.Plan(ctx, request)
	case streamingplan.ServiceID:
		if result = directoryConflict(request); result == nil {
			result, err = streamingplan.Plan(ctx, request)
		}
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
	case replicationplan.ReplicationServiceID:
		result, err = replicationplan.PlanReplications(ctx, request)
	case replicationplan.TransferServiceID:
		result, err = replicationplan.PlanTransfers(ctx, request)
	default:
		err = fmt.Errorf("unsupported YDB planning service")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := provider.ValidatePlannedFeaturesReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
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
	if err := provider.ValidateRenderRequest(described(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if len(request.Changes) > 0 {
		transactional, known := request.Capabilities["transactional_ddl"]
		if !known || transactional {
			return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "native YDB rendering requires established nontransactional DDL")
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
		operation, statement, err := renderChange(change, request.GetCapabilities())
		if err != nil {
			return nil, err
		}
		operations[i], statements[i] = operation, statement
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
	if err := provider.ValidateRenderedBatchReply(described(), request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

// renderChange checks one change of a render request against capabilities and
// returns what renders it: the native operation, or the statement of a common
// object or of a declaration. Its error is the call's status.
func renderChange(change *protocol.Change, capabilities map[string]bool) (*protocol.Envelope, string, error) {
	if common := change.GetCommonObject(); common != nil {
		statement, err := commonobject.Render(common, capabilities)
		if err != nil {
			return nil, "", authored.Status(codes.FailedPrecondition, err)
		}
		return nil, statement, nil
	}
	native := change.GetNative()
	if native.GetRole() != protocol.NativeOperation_STATEMENT || native.GetParent() != nil {
		return nil, "", runtime.ErrorWithReason(codes.InvalidArgument, "only standalone native operations are implemented")
	}
	captured := native.GetValue()
	if err := renderRefusal(captured.GetValue(), capabilities); err != nil {
		return nil, "", err
	}
	normalized, err := normalizeOperation(&protocol.ValueInput{Id: captured.GetId(), Representation: protocol.ValueInput_OPERATION, Value: captured.GetValue()})
	if err != nil {
		return nil, "", authored.Status(codes.InvalidArgument, err)
	}
	if !proto.Equal(normalized, captured) {
		return nil, "", runtime.ErrorWithReason(codes.InvalidArgument, "native operation metadata changed")
	}
	if normalized.Value.Kind == declarationplan.OperationKind {
		statement, err := declarationplan.Text(normalized.Value, capabilities)
		if err != nil {
			return nil, "", authored.Status(codes.FailedPrecondition, err)
		}
		return nil, statement, nil
	}
	return normalized.Value, "", nil
}

// renderRefusal refuses a native operation the target's capabilities cannot
// carry out: a column addition they refuse, or an operation whose kind needs a
// capability the target lacks (see renderGate). Its error is the call's
// status.
func renderRefusal(operation *protocol.Envelope, capabilities map[string]bool) error {
	if operation.GetKind() == tablecreate.InventoryOperationKind {
		change, err := tablecreate.DecodeInventory(operation)
		if err != nil {
			return authored.Status(codes.InvalidArgument, err)
		}
		if err := change.CheckCapabilities(capabilities); err != nil {
			return authored.Status(codes.FailedPrecondition, err)
		}
	}
	if capability, refusal := renderGate(operation.GetKind()); capability != "" && !capabilities[capability] {
		return runtime.ErrorWithReason(codes.FailedPrecondition, refusal)
	}
	return nil
}

// renderGate names the capability a native operation of kind needs before it
// renders, and the refusal of a target without it. A kind no capability gates
// names none.
func renderGate(kind string) (capability, refusal string) {
	switch kind {
	case schema.CoordinationOperation:
		return "coordination_nodes", "native coordination rendering requires established capabilities"
	case schema.SecretOperation:
		return "secrets", "secret rendering requires the secrets capability" + featureflags.Clause("secrets")
	case schema.TopicOperation:
		return topics.CapabilityTopics, "topic rendering requires the topics capability"
	case schema.ChangefeedOperation:
		return changefeeds.CapabilityChangefeeds, "changefeed rendering requires the changefeeds capability"
	case schema.ExternalSourceOperation, schema.ExternalTableOperation:
		return externals.CapabilityExternalDataSources, "external object rendering requires the external_data_sources capability" +
			featureflags.Clause(externals.CapabilityExternalDataSources)
	case schema.StreamingQueryOperation:
		return streaming.Capability, streaming.CapabilityRefusal("streaming query")
	case schema.ReplicationOperation:
		return replication.CapabilityReplication, "async replication rendering requires the async_replication capability" +
			featureflags.Clause(replication.CapabilityReplication)
	case schema.TransferOperation:
		return replication.CapabilityTransfers, "transfer rendering requires the transfers capability" + featureflags.Clause(replication.CapabilityTransfers)
	case schema.WorkloadOperation:
		return workload.CapabilityResourcePools, "resource pools require target capability resource_pools; " + workload.FlagHint
	default:
		return "", ""
	}
}
