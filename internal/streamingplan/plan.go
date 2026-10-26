// Package streamingplan places streaming query stops before their inputs
// change and places creation or restart after those changes.
package streamingplan

import (
	"context"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// ServiceID identifies streaming query planning.
const ServiceID = "ydb-streaming-queries"

func family() objectplan.Family {
	return objectplan.Family{ServiceID: ServiceID, Kind: schema.StreamingQueryProperties, ChangeKind: schema.StreamingQueryChange,
		Capability: streaming.Capability, Noun: "streaming query", StepPrefix: "streaming",
		Strategy: "apply streaming settings in place while preserving topic offsets",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.StreamingQueryProperties, subject)
		},
		NormalizeChange: schema.NormalizeStreamingQueryChange, Operation: schema.StreamingQueryOperationFromChange,
		Present: func(change *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.StreamingQueryOperands(change)
			return before != nil, after != nil, err
		},
	}
}

// Plan validates the entire batch before returning graph contributions. Every
// stop and restart holds an independent precondition and renders one statement.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	result, err := objectplan.Plan(ctx, request, family())
	if err != nil || len(result.GetDiagnostics()) != 0 || len(result.GetContributions()) == 0 {
		return result, err
	}
	base := result.Contributions[0]
	pathRequest, err := objectplan.PathContext(request)
	if err != nil {
		return nil, err
	}
	result.Rewrites = nil
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	for _, step := range base.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index := -1
		for i, receipt := range result.Changes {
			if proto.Equal(receipt.Steps[0], step.Id) {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("streaming planning lost a change receipt")
		}
		receipt := result.Changes[index]
		change, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(step.Operation.Value.Value))
		if err != nil {
			return nil, err
		}
		parts, err := streaming.Steps(change)
		if err != nil {
			return nil, err
		}
		receipt.Steps = nil
		for ordinal, part := range parts {
			id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%d", step.Id.Name, ordinal)}
			value, err := operation(id.Name, step.Operation.Value.NormalizedOwner, part)
			if err != nil {
				return nil, err
			}
			action := protocol.PlanningEffect_ALTER
			if part.Before == nil {
				action = protocol.PlanningEffect_CREATE
			} else if part.After == nil {
				action = protocol.PlanningEffect_DROP
			}
			edges, err := objectplan.PathDependencies(pathRequest, family(), receipt.Subject, action, id)
			if err != nil {
				return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_INVALID_SCHEMA, Kind: schema.StreamingQueryChange, Message: err.Error(), Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}, nil
			}
			contribution.Dependencies = append(contribution.Dependencies, edges...)
			metadata := &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: value.OperationSemantics.Impact, Reason: value.OperationSemantics.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(receipt.Subject), Action: action}, {Subject: objectplan.Slot(receipt.Subject), Action: action}}}
			access, err := objectplan.OrderAccess(request, family(), receipt.Subject, action, id)
			if err != nil {
				return nil, err
			}
			metadata.Effects = append(metadata.Effects, access.Effects...)
			contribution.Dependencies = append(contribution.Dependencies, access.Dependencies...)
			// Streaming preconditions contain only the persistent query settings.
			// Principal edits do not change that capture; matching access writes
			// still follow the query operation through OrderAccess.
			if len(access.Sources) != 0 {
				result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
			}
			spec := part.After
			if streaming.Early(part) {
				metadata.Placement = protocol.PlanningMetadata_EARLY
				spec = part.Before
			}
			reads, dependencies, err := queryResources(pathRequest, receipt.Subject, *spec, streaming.Early(part), id)
			if err != nil {
				return nil, err
			}
			metadata.Effects = append(metadata.Effects, reads...)
			contribution.Dependencies = append(contribution.Dependencies, dependencies...)
			if len(receipt.Steps) > 0 {
				contribution.Dependencies = append(contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(receipt.Steps[len(receipt.Steps)-1]), After: proto.CloneOf(id)})
			}
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: id, Metadata: metadata, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value, Phase: protocol.PlanningOperation_LIFECYCLE}})
			receipt.Steps = append(receipt.Steps, proto.CloneOf(id))
		}
	}
	result.Contributions = []*protocol.PlanningContribution{contribution}
	return result, nil
}

func operation(id string, owner *protocol.Reference, change streaming.Change) (*protocol.NormalizedValue, error) {
	var before, after *protocol.Envelope
	var err error
	if change.Before != nil {
		before, err = schema.CaptureStreamingQueryIntent(owner, *change.Before, false)
		if err != nil {
			return nil, err
		}
		before, err = schema.ConvertNative(before, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
		if err != nil {
			return nil, err
		}
	}
	if change.After != nil {
		after, err = schema.CaptureStreamingQueryIntent(owner, *change.After, change.AllowStateReset)
		if err != nil {
			return nil, err
		}
	}
	captured, err := schema.CaptureStreamingQueryChange(id, owner, before, after)
	if err != nil {
		return nil, err
	}
	return schema.StreamingQueryOperationFromChange(id, captured.Value)
}
