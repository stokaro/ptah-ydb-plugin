// Package objectplan lowers captured changes of standalone YDB objects, such
// as coordination nodes, into steps of the host's shared graph: one native
// operation per change, ordered against the common steps and captured tables
// that occupy the same scheme path.
package objectplan

import (
	"context"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// PathKind identifies the physical namespace shared by native YDB objects.
const PathKind = "ydb.scheme-path"

// Family is what one kind of standalone object brings to planning. Every
// field but Refuse is required.
type Family struct {
	// ServiceID is the planning service the host asks for the family's changes.
	ServiceID string
	// Kind is the object model kind a change's subject names.
	Kind string
	// ChangeKind is the change model the service plans.
	ChangeKind string
	// Capability is the session fact the family's operations require.
	Capability string
	// Noun names one object in a refusal, such as "coordination node".
	Noun string
	// StepPrefix starts the name of each step, such as "coordination".
	StepPrefix string
	// Strategy says how a receipt carries the change out.
	Strategy string
	// Owner validates a subject of the family and returns its owner.
	Owner func(*protocol.ComparisonIdentity) (*protocol.Reference, error)
	// NormalizeChange re-normalizes a captured change.
	NormalizeChange func(*protocol.ValueInput) (*protocol.NormalizedValue, error)
	// Operation lowers a normalized change to its operation. An error is a
	// refusal of the change, not of the request.
	Operation func(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error)
	// Present reports which operands of a normalized change are present.
	Present func(change *protocol.Envelope) (before, after bool, err error)
	// Refuse says why a normalized change cannot be planned on a target
	// holding capabilities, or returns nil; a nil Refuse refuses nothing.
	// Its error is a refusal of the change, not of the request.
	Refuse func(change *protocol.Envelope, capabilities map[string]bool) error
	// RefuseInPlan says why a normalized change cannot be planned beside the
	// captured tables and common steps of request, or returns nil; a nil
	// RefuseInPlan refuses nothing. Its error is a refusal of the change.
	RefuseInPlan func(request *protocol.PlanFeaturesRequest, change *protocol.Envelope) error
	// Paths names the scheme paths of the session's database a normalized
	// change acts on beyond its own, such as the replica tables an async
	// replication creates, each with its action. A nil Paths names none.
	Paths func(request *protocol.PlanFeaturesRequest, change *protocol.Envelope) ([]PathEffect, error)
}

// PathEffect is one scheme path a change acts on beyond its own: a path
// relative to the database root, as a directory and a leaf name, and what the
// change does there.
//
// Subject, when set, names the object at the path by its own identity
// instead, such as a topic or a changefeed. The host orders a step that acts
// on such a subject against the steps of other planning services that act on
// the same one, and no common step names it, so it orders nothing against
// the common steps. Schema and Name are then unused.
type PathEffect struct {
	Schema, Name string
	Action       protocol.PlanningEffect_Action
	Subject      *protocol.ComparisonIdentity
}

type change struct {
	index  int
	input  *protocol.PlanningChange
	value  *protocol.NormalizedValue
	action protocol.PlanningEffect_Action
}

// Plan contributes operations, footprints, dependencies, and a receipt per input.
// The SDK boundary validates transport ownership; this service validates meaning.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest, family Family) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if refused, err := checkAssignment(request, family); refused != nil || err != nil {
		return refused, err
	}
	changes := make([]change, 0, len(request.Changes))
	for i, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, refused, err := captureChange(request, family, i, input)
		if refused != nil || err != nil {
			return refused, err
		}
		changes = append(changes, item)
	}
	slices.SortFunc(changes, func(a, b change) int { return provider.CompareIdentities(a.input.Subject, b.input.Subject) })
	result := &protocol.PlannedFeatures{Complete: true, Changes: make([]*protocol.PlanningReceipt, len(changes))}
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	for i, item := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", family.StepPrefix, i)}
		item.value.Id = id.Name
		if refused := planStep(request, family, item, id, result, contribution); refused != nil {
			return refused, nil
		}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// checkAssignment checks that request is the family's planning assignment
// and that its identifier context is one YDB plans in. A target without the
// family's capability or without nontransactional DDL, or one that did not
// establish whether DDL is transactional, gets a refusal of the request; any
// other mismatch is an error. It returns neither when planning can go on.
func checkAssignment(request *protocol.PlanFeaturesRequest, family Family) (*protocol.PlannedFeatures, error) {
	if request.PlanningId != family.ServiceID || len(request.ParentKinds) != 0 {
		return nil, fmt.Errorf("invalid %s planning assignment", family.Noun)
	}
	if len(request.Changes) > 0 && (!request.Capabilities[family.Capability] || request.Capabilities["transactional_ddl"]) {
		return refusal(family, 0, fmt.Sprintf("Planning a %s requires the %s capability and nontransactional DDL%s", family.Noun, family.Capability,
			featureflags.Clause(family.Capability))), nil
	}
	if _, known := request.Capabilities["transactional_ddl"]; len(request.Changes) > 0 && !known {
		return refusal(family, 0, "The DDL transaction capability was not established"), nil
	}
	if request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return nil, fmt.Errorf("invalid YDB planning identifier context")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return nil, fmt.Errorf("YDB planning requires exact identifiers")
		}
	}
	return nil, nil
}

// captureChange re-normalizes input, the change at index i of request, and
// lowers it to its operation with the action its operands imply: a creation
// without a current operand, a drop without a desired one, an alteration
// otherwise. A change the family refuses, or cannot lower, returns a refusal;
// a forged receipt or a change of another database returns an error.
func captureChange(request *protocol.PlanFeaturesRequest, family Family, i int, input *protocol.PlanningChange) (change, *protocol.PlannedFeatures, error) {
	owner, err := family.Owner(input.Subject)
	if err != nil {
		return change{}, nil, err
	}
	captured, err := family.NormalizeChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
	if err != nil {
		return change{}, nil, err
	}
	if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
		return change{}, nil, fmt.Errorf("planning change has a forged normalization receipt or identity")
	}
	if request.DatabasePath != "" && owner.Database != "" && owner.Database != request.DatabasePath {
		return change{}, nil, fmt.Errorf("%s change targets another database", family.Noun)
	}
	if family.Refuse != nil {
		if err := family.Refuse(captured.Value, request.Capabilities); err != nil {
			return change{}, refusal(family, i, err.Error()), nil
		}
	}
	if family.RefuseInPlan != nil {
		if err := family.RefuseInPlan(request, captured.Value); err != nil {
			return change{}, refusal(family, i, err.Error()), nil
		}
	}
	value, err := family.Operation("pending", captured.Value)
	if err != nil {
		return change{}, refusal(family, i, err.Error()), nil
	}
	before, after, err := family.Present(captured.Value)
	if err != nil {
		return change{}, nil, err
	}
	action := protocol.PlanningEffect_ALTER
	if !before {
		action = protocol.PlanningEffect_CREATE
	} else if !after {
		action = protocol.PlanningEffect_DROP
	}
	return change{i, input, value, action}, nil, nil
}

// planStep adds the step id that carries out item to contribution, with its
// effects and its dependencies on the common steps and captured tables of
// request, and records the change's receipt in result. A step ordered against
// access needs the planning.lifecycle target feature, and one that accounts
// for revokes replaces them in a rewrite. A failure returns a refusal of the
// change.
func planStep(
	request *protocol.PlanFeaturesRequest,
	family Family,
	item change,
	id *protocol.StepIdentity,
	result *protocol.PlannedFeatures,
	contribution *protocol.PlanningContribution,
) *protocol.PlannedFeatures {
	access, err := OrderAccess(request, family, item.input.Subject, item.action, id)
	if err != nil {
		return refusal(family, item.index, err.Error())
	}
	access.Dependencies = append(access.Dependencies, BeforePrincipalChanges(request, item.action, id)...)
	below, err := AfterCreationBelowDirectoryAccess(request, item.action, item.input.Subject, id)
	if err != nil {
		return refusal(family, item.index, err.Error())
	}
	access.Dependencies = append(access.Dependencies, below...)
	pathRequest, err := PathContext(request)
	if err != nil {
		return refusal(family, item.index, err.Error())
	}
	edges, err := dependencies(pathRequest, family, item, id)
	if err != nil {
		return refusal(family, item.index, err.Error())
	}
	// Native objects share the lifecycle window so a path can pass from
	// a dropped table or another kind of object to its replacement.
	facts := item.value.OperationSemantics
	step := &protocol.PlanningStep{
		Id:        id,
		Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.value, Phase: protocol.PlanningOperation_LIFECYCLE},
		Metadata: &protocol.PlanningMetadata{
			Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
			Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(item.input.Subject), Action: item.action},
				{Subject: slot(item.input.Subject), Action: item.action},
			},
		},
	}
	if len(access.Dependencies)+len(access.Sources) != 0 {
		if !slices.Contains(request.GetTargetFeatures(), provider.PlanningLifecycleFeature) {
			return refusal(family, item.index, "Native access ordering requires planning.lifecycle")
		}
		step.Operation.Phase = protocol.PlanningOperation_LIFECYCLE
		step.Metadata.Effects = append(step.Metadata.Effects, access.Effects...)
		contribution.Dependencies = append(contribution.Dependencies, access.Dependencies...)
		if len(access.Sources) != 0 {
			result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
		}
	}
	if family.Paths != nil {
		effects, ordered, err := sidePaths(pathRequest, family, item, id)
		if err != nil {
			return refusal(family, item.index, err.Error())
		}
		step.Metadata.Effects = append(step.Metadata.Effects, effects...)
		edges = append(edges, ordered...)
	}
	contribution.Steps = append(contribution.Steps, step)
	contribution.Dependencies = append(contribution.Dependencies, edges...)
	result.Changes[item.index] = &protocol.PlanningReceipt{
		Subject: proto.CloneOf(item.input.Subject), Kind: family.ChangeKind, Strategy: family.Strategy, Steps: []*protocol.StepIdentity{proto.CloneOf(id)},
	}
	return nil
}

func refusal(family Family, index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{
		Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: family.ChangeKind, Message: message,
		Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)},
	}}}
}
