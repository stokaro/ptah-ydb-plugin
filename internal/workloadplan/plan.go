// Package workloadplan lowers captured resource pool and classifier changes
// into the host's shared graph: one step that carries every workload change
// of the plan, so that a classifier trading ranks with another runs as the
// drop, the moves and the creation YDB needs, in that order.
package workloadplan

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the workload planner.
const ServiceID = "ydb-workload"

// StepName is the name of the one step a plan's workload changes run in.
const StepName = "workload/000000"

// strategy is how a receipt says a workload change is carried out.
const strategy = "apply captured workload settings after releasing occupied classifier ranks"

// ChangeKinds are the changes the planner takes.
func ChangeKinds() []string {
	return []string{schema.ResourcePoolChange, schema.ResourcePoolClassifierChange}
}

type change struct {
	index      int
	input      *protocol.PlanningChange
	classifier bool
	action     protocol.PlanningEffect_Action
}

// Plan contributes the workload step, its effects and the order against the
// roles a classifier may name, and a receipt per change. A change YDB cannot
// make, or a target without resource pools, is refused with a diagnostic and
// plans nothing.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.PlanningId != ServiceID || len(request.ParentKinds) != 0 {
		return nil, fmt.Errorf("invalid workload planning assignment")
	}
	if request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return nil, fmt.Errorf("invalid YDB planning identifier context")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return nil, fmt.Errorf("YDB planning requires exact identifiers")
		}
	}
	if len(request.Changes) == 0 {
		return &protocol.PlannedFeatures{Complete: true}, nil
	}
	if !request.Capabilities[workload.CapabilityResourcePools] {
		return refusal(request, 0, "this target does not support resource pools and classifiers; "+workload.FlagHint), nil
	}
	if transactional, known := request.Capabilities["transactional_ddl"]; !known || transactional {
		return refusal(request, 0, "Workload DDL cannot run in a SQL transaction, and the target's DDL transaction capability was not established as nontransactional"), nil
	}
	var changes schema.WorkloadChanges
	var planned []change
	for i, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, err := capture(request, i, input, &changes)
		if err != nil {
			return nil, err
		}
		planned = append(planned, item)
	}
	slices.SortFunc(changes.Pools, func(a, b workload.PoolChange) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(changes.Classifiers, func(a, b workload.ClassifierChange) int { return cmp.Compare(a.Name, b.Name) })
	if _, err := workload.Statements(changes.Pools, changes.Classifiers); err != nil {
		return refusal(request, refusedIndex(planned, err), err.Error()), nil
	}
	operation, err := schema.CaptureWorkloadOperation(StepName, "", changes)
	if err != nil {
		return nil, err
	}
	id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: StepName}
	facts := operation.OperationSemantics
	metadata := &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason}
	result := &protocol.PlannedFeatures{Complete: true, Changes: make([]*protocol.PlanningReceipt, len(request.Changes))}
	for _, item := range planned {
		metadata.Effects = append(metadata.Effects, &protocol.PlanningEffect{Subject: proto.CloneOf(item.input.Subject), Action: item.action})
		result.Changes[item.index] = &protocol.PlanningReceipt{Subject: proto.CloneOf(item.input.Subject), Kind: item.input.Value.GetValue().GetKind(),
			Strategy: strategy, Steps: []*protocol.StepIdentity{proto.CloneOf(id)}}
	}
	slices.SortFunc(metadata.Effects, func(a, b *protocol.PlanningEffect) int { return provider.CompareIdentities(a.Subject, b.Subject) })
	step := &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation}, Metadata: metadata}
	result.Contributions = []*protocol.PlanningContribution{{Owner: schema.ProviderID, Steps: []*protocol.PlanningStep{step}, Dependencies: principalDependencies(id, request.CommonSteps)}}
	return result, ctx.Err()
}

// capture validates one planning change and adds it to changes.
func capture(request *protocol.PlanFeaturesRequest, index int, input *protocol.PlanningChange, changes *schema.WorkloadChanges) (change, error) {
	kind := schema.ResourcePoolProperties
	classifier := input.GetValue().GetValue().GetKind() == schema.ResourcePoolClassifierChange
	if classifier {
		kind = schema.ResourcePoolClassifierProperties
	}
	owner, err := schema.WorkloadBoundOwner(kind, input.Subject)
	if err != nil {
		return change{}, err
	}
	captured, err := schema.NormalizeWorkloadChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
	if err != nil {
		return change{}, err
	}
	if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
		return change{}, fmt.Errorf("planning change has a forged normalization receipt or identity")
	}
	if request.DatabasePath != "" && owner.Database != "" && owner.Database != request.DatabasePath {
		return change{}, fmt.Errorf("workload change targets another database")
	}
	item := change{index: index, input: input, classifier: classifier, action: protocol.PlanningEffect_ALTER}
	var before, after bool
	if classifier {
		value, err := schema.ClassifierChangeOf(captured.Value)
		if err != nil {
			return change{}, err
		}
		if slices.ContainsFunc(changes.Classifiers, func(have workload.ClassifierChange) bool { return have.Name == value.Name }) {
			return change{}, fmt.Errorf("duplicate workload change")
		}
		changes.Classifiers = append(changes.Classifiers, value)
		before, after = value.Before != nil, value.After != nil
	} else {
		value, err := schema.PoolChangeOf(captured.Value)
		if err != nil {
			return change{}, err
		}
		if slices.ContainsFunc(changes.Pools, func(have workload.PoolChange) bool { return have.Name == value.Name }) {
			return change{}, fmt.Errorf("duplicate workload change")
		}
		changes.Pools = append(changes.Pools, value)
		before, after = value.Before != nil, value.After != nil
	}
	switch {
	case !before:
		item.action = protocol.PlanningEffect_CREATE
	case !after:
		item.action = protocol.PlanningEffect_DROP
	}
	return item, nil
}

// refusedIndex is the input of the change err names, or the first one.
func refusedIndex(planned []change, err error) int {
	var refused *workload.ChangeError
	if errors.As(err, &refused) {
		for _, item := range planned {
			if item.classifier == refused.Classifier && item.input.Subject.GetName().GetNormalized() == refused.Name {
				return item.index
			}
		}
	}
	return 0
}

// principalDependencies orders the step after every common step that creates
// or changes a role and before every one that drops a role: a classifier names
// a user or a group as its member. The member is not a read effect, because
// YDB takes a classifier whose member does not exist and routes nobody by it.
func principalDependencies(id *protocol.StepIdentity, common []*protocol.CommonPlanningStep) []*protocol.PlanningDependency {
	var edges []*protocol.PlanningDependency
	for _, step := range common {
		for _, effect := range step.GetMetadata().GetEffects() {
			if effect.GetSubject().GetKind() != "role" || effect.GetAction() == protocol.PlanningEffect_READ {
				continue
			}
			edge := &protocol.PlanningDependency{Before: proto.CloneOf(step.Id), After: proto.CloneOf(id)}
			if effect.GetAction() == protocol.PlanningEffect_DROP {
				edge = &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(step.Id)}
			}
			if !slices.ContainsFunc(edges, func(have *protocol.PlanningDependency) bool { return proto.Equal(have, edge) }) {
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

func refusal(request *protocol.PlanFeaturesRequest, index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
		Kind: request.Changes[index].GetValue().GetValue().GetKind(), Message: message, Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}
}
