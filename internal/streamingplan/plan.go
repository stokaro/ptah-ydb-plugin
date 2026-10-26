// Package streamingplan places streaming query stops before their inputs
// change and places creation or restart after those changes.
package streamingplan

import (
	"context"
	"fmt"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
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
			edges, err := objectplan.PathDependencies(request, family(), receipt.Subject, action, id)
			if err != nil {
				return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_INVALID_SCHEMA, Kind: schema.StreamingQueryChange, Message: err.Error(), Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}, nil
			}
			contribution.Dependencies = append(contribution.Dependencies, edges...)
			metadata := &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: value.OperationSemantics.Impact, Reason: value.OperationSemantics.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(receipt.Subject), Action: action}, {Subject: objectplan.Slot(receipt.Subject), Action: action}}}
			var body string
			if streaming.Early(part) {
				metadata.Placement = protocol.PlanningMetadata_EARLY
				body = part.Before.Text
			} else {
				body = part.After.Text
			}
			metadata.Effects = append(metadata.Effects, topicReads(body, receipt.Subject)...)
			for _, common := range request.CommonSteps {
				edge := &protocol.PlanningDependency{Before: proto.CloneOf(common.Id), After: proto.CloneOf(id)}
				if streaming.Early(part) {
					edge.Before, edge.After = edge.After, edge.Before
				}
				contribution.Dependencies = append(contribution.Dependencies, edge)
			}
			if len(receipt.Steps) > 0 {
				contribution.Dependencies = append(contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(receipt.Steps[len(receipt.Steps)-1]), After: proto.CloneOf(id)})
			}
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: id, Metadata: metadata, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value}})
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
		before, err = schema.CaptureObject(owner, &native.StreamingQuerySettings{Text: change.Before.Text, Run: change.Before.Run, ResourcePool: change.Before.ResourcePool})
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

func topicReads(body string, subject *protocol.ComparisonIdentity) []*protocol.PlanningEffect {
	var effects []*protocol.PlanningEffect
	for _, name := range streaming.NamedPaths(body) {
		directory := ""
		if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
			directory, name = name[:slash], name[slash+1:]
		}
		if err := streaming.ValidateIdentity(directory, name); err != nil {
			continue
		}
		ref := proto.CloneOf(subject)
		ref.Kind, ref.Database = schema.TopicProperties, &protocol.ComparisonPart{}
		ref.Schema = &protocol.ComparisonPart{Source: directory, Normalized: directory}
		ref.Name = &protocol.ComparisonPart{Source: name, Normalized: name}
		effects = append(effects, &protocol.PlanningEffect{Subject: ref, Action: protocol.PlanningEffect_READ})
	}
	return effects
}
