// Package streamingplan places streaming query stops before their inputs
// change and places creation or restart after those changes.
package streamingplan

import (
	"context"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
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
	if !slices.Contains(request.GetTargetFeatures(), provider.PlanningObjectContextFeature) || !slices.Equal(request.ContextKinds, []string{schema.StreamingQueryProperties}) {
		return nil, fmt.Errorf("streaming planning requires complete planning.object-context assignment")
	}
	result, err := objectplan.Plan(ctx, request, family())
	if err != nil || len(result.GetDiagnostics()) != 0 {
		return result, err
	}
	var base []*protocol.PlanningStep
	for _, contribution := range result.Contributions {
		base = append(base, contribution.Steps...)
	}
	pathRequest, err := objectplan.PathContext(request)
	if err != nil {
		return nil, err
	}
	result.Rewrites = nil
	builder := &lifecycleBuilder{request: request, paths: pathRequest, contribution: &protocol.PlanningContribution{Owner: schema.ProviderID}}
	for _, step := range base {
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
		receipt.Steps, err = builder.add(step.Id.Name, receipt.Subject, step.Operation.Value.NormalizedOwner, step.Operation.Value.Value.GetContext(), parts)
		if err != nil {
			return nil, err
		}
	}
	if err := assessObjects(ctx, request, result, builder); err != nil {
		return nil, err
	}
	if len(result.Diagnostics) != 0 {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: result.Diagnostics}, nil
	}
	result.Contributions = nil
	if len(builder.contribution.Steps) != 0 {
		result.Contributions = []*protocol.PlanningContribution{builder.contribution}
	}
	result.Rewrites = builder.rewrites
	return result, nil
}

func operation(id string, owner *protocol.Reference, bound *protocol.DatabaseContext, change streaming.Change) (*protocol.NormalizedValue, error) {
	var before, after *protocol.Envelope
	var err error
	if change.Before != nil {
		before, err = schema.CaptureStreamingQueryIntent(owner, *change.Before, false)
		if err != nil {
			return nil, err
		}
		before.Context = proto.CloneOf(bound)
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
		after.Context = proto.CloneOf(bound)
	}
	captured, err := schema.CaptureStreamingQueryChange(id, owner, before, after)
	if err != nil {
		return nil, err
	}
	return schema.StreamingQueryOperationFromChange(id, captured.Value)
}
