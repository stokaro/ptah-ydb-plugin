// Package workloadplan lowers captured resource pool and classifier changes
// into the host's shared graph: one step per statement, chained in the order
// YDB needs, so that a classifier trading ranks with another runs as the
// drop, the moves and the creation, in that order. Each step runs where the
// YDB engine built into the host ran its workload statements; see
// [objectplan.FollowLateInOrder].
package workloadplan

import (
	"context"
	"errors"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the workload planner.
const ServiceID = "ydb-workload"

// StepPrefix starts the name of each workload step, which ends in the step's
// position among the plan's workload statements, such as workload/000001.
const StepPrefix = "workload"

// strategy is how a receipt says a workload change is carried out.
const strategy = "apply captured workload settings after releasing occupied classifier ranks"

// ChangeKinds are the changes the planner takes.
func ChangeKinds() []string {
	return []string{schema.ResourcePoolChange, schema.ResourcePoolClassifierChange}
}

// change is one planning change: its position in the request, and the name
// of the pool or classifier it changes.
type change struct {
	index      int
	input      *protocol.PlanningChange
	classifier bool
	name       string
}

// Plan contributes a step per workload statement, chained in the order the
// statements run, with the effects and the order against the roles a
// classifier may name and the access on a pool, and a receipt per change
// naming the steps that carry it out. A change YDB cannot make, or a target
// without resource pools, is refused with a diagnostic and plans nothing.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAssignment(request); err != nil {
		return nil, err
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
	operations, err := workload.Operations(changes.Pools, changes.Classifiers)
	if err != nil {
		return refusal(request, refusedIndex(planned, err), err.Error()), nil
	}
	result := &protocol.PlannedFeatures{Complete: true, Changes: make([]*protocol.PlanningReceipt, len(request.Changes))}
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	for i, operation := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", StepPrefix, i)}
		if refused, err := planStep(request, planned, operation, id, result, contribution); refused != nil || err != nil {
			return refused, err
		}
		if i > 0 {
			contribution.Dependencies = append(contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(contribution.Steps[i-1].Id), After: proto.CloneOf(id)})
		}
	}
	for _, receipt := range result.Changes {
		if receipt == nil {
			return nil, errors.New("workload planning lost a change receipt")
		}
	}
	objectplan.FollowLateInOrder(request, contribution, result.Rewrites)
	result.Contributions = []*protocol.PlanningContribution{contribution}
	return result, ctx.Err()
}

// planStep adds the step id that runs operation to contribution, with the
// order against the roles and the access the operation's object needs, and
// names the step in the receipt of the change the operation carries out. A
// change whose access cannot be ordered returns a refusal.
func planStep(request *protocol.PlanFeaturesRequest, planned []change, operation workload.Operation, id *protocol.StepIdentity,
	result *protocol.PlannedFeatures, contribution *protocol.PlanningContribution,
) (*protocol.PlannedFeatures, error) {
	item, alone, err := operationChange(planned, operation)
	if err != nil {
		return nil, err
	}
	value, err := schema.CaptureWorkloadOperation(id.Name, "", alone)
	if err != nil {
		return nil, err
	}
	action := operationAction(operation)
	noun := "resource pool"
	if item.classifier {
		noun = "resource pool classifier"
	}
	access, err := objectplan.OrderAccess(request, objectplan.Family{Noun: noun}, item.input.Subject, action, id)
	if err != nil {
		return refusal(request, item.index, err.Error()), nil
	}
	if len(access.Dependencies)+len(access.Sources) != 0 && !slices.Contains(request.GetTargetFeatures(), provider.PlanningLifecycleFeature) {
		return refusal(request, item.index, "Native access ordering requires planning.lifecycle"), nil
	}
	facts := value.OperationSemantics
	effects := append([]*protocol.PlanningEffect{{Subject: proto.CloneOf(item.input.Subject), Action: action}}, access.Effects...)
	// The steps share the lifecycle window with the host's common steps, so
	// that each can run after the common steps that need none of them.
	contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: id,
		Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value, Phase: protocol.PlanningOperation_LIFECYCLE},
		Metadata:  &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects}})
	contribution.Dependencies = append(contribution.Dependencies, principalDependencies(id, request.CommonSteps)...)
	contribution.Dependencies = append(contribution.Dependencies, access.Dependencies...)
	if len(access.Sources) != 0 {
		result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
	}
	receipt := result.Changes[item.index]
	if receipt == nil {
		receipt = &protocol.PlanningReceipt{Subject: proto.CloneOf(item.input.Subject), Kind: item.input.Value.GetValue().GetKind(), Strategy: strategy}
		result.Changes[item.index] = receipt
	}
	receipt.Steps = append(receipt.Steps, proto.CloneOf(id))
	return nil, nil
}

// operationChange returns the planned change whose object operation changes
// and the workload changes that hold operation's change alone.
func operationChange(planned []change, operation workload.Operation) (change, schema.WorkloadChanges, error) {
	var alone schema.WorkloadChanges
	name := ""
	if operation.Pool != nil {
		alone.Pools, name = []workload.PoolChange{*operation.Pool}, operation.Pool.Name
	} else {
		alone.Classifiers, name = []workload.ClassifierChange{*operation.Classifier}, operation.Classifier.Name
	}
	for _, item := range planned {
		if item.classifier == (operation.Classifier != nil) && item.name == name {
			return item, alone, nil
		}
	}
	return change{}, alone, fmt.Errorf("workload operation names no planned change")
}

// operationAction is what operation does to its object.
func operationAction(operation workload.Operation) protocol.PlanningEffect_Action {
	before, after := operation.Pool != nil && operation.Pool.Before != nil, operation.Pool != nil && operation.Pool.After != nil
	if operation.Classifier != nil {
		before, after = operation.Classifier.Before != nil, operation.Classifier.After != nil
	}
	switch {
	case !before:
		return protocol.PlanningEffect_CREATE
	case !after:
		return protocol.PlanningEffect_DROP
	}
	return protocol.PlanningEffect_ALTER
}

// validateAssignment refuses a request addressed to another planner or naming
// parent kinds, and an identifier context that sets a default schema or a
// catalog collation or resolves a name to another spelling.
func validateAssignment(request *protocol.PlanFeaturesRequest) error {
	if request.PlanningId != ServiceID || len(request.ParentKinds) != 0 {
		return fmt.Errorf("invalid workload planning assignment")
	}
	if request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return fmt.Errorf("invalid YDB planning identifier context")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return fmt.Errorf("YDB planning requires exact identifiers")
		}
	}
	return nil
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
	item := change{index: index, input: input, classifier: classifier}
	if classifier {
		value, err := schema.ClassifierChangeOf(captured.Value)
		if err != nil {
			return change{}, err
		}
		if slices.ContainsFunc(changes.Classifiers, func(have workload.ClassifierChange) bool { return have.Name == value.Name }) {
			return change{}, fmt.Errorf("duplicate workload change")
		}
		changes.Classifiers = append(changes.Classifiers, value)
		item.name = value.Name
	} else {
		value, err := schema.PoolChangeOf(captured.Value)
		if err != nil {
			return change{}, err
		}
		if slices.ContainsFunc(changes.Pools, func(have workload.PoolChange) bool { return have.Name == value.Name }) {
			return change{}, fmt.Errorf("duplicate workload change")
		}
		changes.Pools = append(changes.Pools, value)
		item.name = value.Name
	}
	return item, nil
}

// refusedIndex is the input of the change err names, or the first one.
func refusedIndex(planned []change, err error) int {
	if refused, ok := errors.AsType[*workload.ChangeError](err); ok {
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
	// #nosec G115 -- index is a position in the request, which runtime.MaxMessageBytes bounds to 4 MiB
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
		Kind: request.Changes[index].GetValue().GetValue().GetKind(), Message: message, Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}
}
