// Package coordinationplan lowers captured coordination changes into a shared graph.
package coordinationplan

import (
	"context"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the native coordination planner.
const ServiceID = "ydb-coordination"

// PathKind identifies the physical namespace shared by native YDB objects.
const PathKind = "ydb.scheme-path"

type change struct {
	index  int
	input  *protocol.PlanningChange
	value  *protocol.NormalizedValue
	action protocol.PlanningEffect_Action
}

// Plan contributes operations, footprints, dependencies, and a receipt per input.
// The SDK boundary validates transport ownership; this service validates meaning.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.PlanningId != ServiceID || len(request.ParentKinds) != 0 {
		return nil, fmt.Errorf("invalid coordination planning assignment")
	}
	if len(request.Changes) > 0 && (!request.Capabilities["coordination_nodes"] || request.Capabilities["transactional_ddl"]) {
		return refusal(0, "Coordination planning requires native coordination support and nontransactional DDL"), nil
	}
	if _, known := request.Capabilities["transactional_ddl"]; len(request.Changes) > 0 && !known {
		return refusal(0, "The DDL transaction capability was not established"), nil
	}
	if request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return nil, fmt.Errorf("invalid YDB planning identifier context")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return nil, fmt.Errorf("YDB planning requires exact identifiers")
		}
	}
	changes := make([]change, 0, len(request.Changes))
	for i, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner, err := schema.CoordinationBoundOwner(input.Subject)
		if err != nil {
			return nil, err
		}
		captured, err := schema.NormalizeCoordinationChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
		if err != nil {
			return nil, err
		}
		if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
			return nil, fmt.Errorf("planning change has a forged normalization receipt or identity")
		}
		if request.DatabasePath != "" && owner.Database != "" && owner.Database != request.DatabasePath {
			return nil, fmt.Errorf("coordination change targets another database")
		}
		value, err := schema.CoordinationOperationFromChange("pending", captured.Value)
		if err != nil {
			return refusal(i, err.Error()), nil
		}
		before, after, err := schema.CoordinationOperands(captured.Value)
		if err != nil {
			return nil, err
		}
		action := protocol.PlanningEffect_ALTER
		if before == nil {
			action = protocol.PlanningEffect_CREATE
		} else if after == nil {
			action = protocol.PlanningEffect_DROP
		}
		changes = append(changes, change{i, input, value, action})
	}
	slices.SortFunc(changes, func(a, b change) int { return provider.CompareIdentities(a.input.Subject, b.input.Subject) })
	result := &protocol.PlannedFeatures{Complete: true, Changes: make([]*protocol.PlanningReceipt, len(changes))}
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	for i, item := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("coordination/%06d", i)}
		item.value.Id = id.Name
		edges, err := dependencies(request, item, id)
		if err != nil {
			return refusal(item.index, err.Error()), nil
		}
		facts := item.value.OperationSemantics
		step := &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.value}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.input.Subject), Action: item.action}, {Subject: slot(item.input.Subject), Action: item.action}}}}
		contribution.Steps = append(contribution.Steps, step)
		contribution.Dependencies = append(contribution.Dependencies, edges...)
		result.Changes[item.index] = &protocol.PlanningReceipt{Subject: proto.CloneOf(item.input.Subject), Kind: schema.CoordinationChange, Strategy: "Apply the captured configuration through the native coordination service", Steps: []*protocol.StepIdentity{proto.CloneOf(id)}}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func refusal(index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: schema.CoordinationChange, Message: message, Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}
}
