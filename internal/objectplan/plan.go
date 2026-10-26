// Package objectplan lowers captured changes of standalone YDB objects, such
// as coordination nodes, into steps of the host's shared graph: one native
// operation per change, ordered against the common steps and captured tables
// that occupy the same scheme path.
package objectplan

import (
	"context"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
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
	if request.PlanningId != family.ServiceID || len(request.ParentKinds) != 0 {
		return nil, fmt.Errorf("invalid %s planning assignment", family.Noun)
	}
	if len(request.Changes) > 0 && (!request.Capabilities[family.Capability] || request.Capabilities["transactional_ddl"]) {
		return refusal(family, 0, fmt.Sprintf("Planning a %s requires the %s capability and nontransactional DDL", family.Noun, family.Capability)), nil
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
	changes := make([]change, 0, len(request.Changes))
	for i, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner, err := family.Owner(input.Subject)
		if err != nil {
			return nil, err
		}
		captured, err := family.NormalizeChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
		if err != nil {
			return nil, err
		}
		if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
			return nil, fmt.Errorf("planning change has a forged normalization receipt or identity")
		}
		if request.DatabasePath != "" && owner.Database != "" && owner.Database != request.DatabasePath {
			return nil, fmt.Errorf("%s change targets another database", family.Noun)
		}
		if family.Refuse != nil {
			if err := family.Refuse(captured.Value, request.Capabilities); err != nil {
				return refusal(family, i, err.Error()), nil
			}
		}
		value, err := family.Operation("pending", captured.Value)
		if err != nil {
			return refusal(family, i, err.Error()), nil
		}
		before, after, err := family.Present(captured.Value)
		if err != nil {
			return nil, err
		}
		action := protocol.PlanningEffect_ALTER
		if !before {
			action = protocol.PlanningEffect_CREATE
		} else if !after {
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
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", family.StepPrefix, i)}
		item.value.Id = id.Name
		access, err := OrderAccess(request, family, item.input.Subject, item.action, id)
		if err != nil {
			return refusal(family, item.index, err.Error()), nil
		}
		access.Dependencies = append(access.Dependencies, BeforePrincipalChanges(request, item.action, id)...)
		pathRequest, err := PathContext(request)
		if err != nil {
			return refusal(family, item.index, err.Error()), nil
		}
		edges, err := dependencies(pathRequest, family, item, id)
		if err != nil {
			return refusal(family, item.index, err.Error()), nil
		}
		facts := item.value.OperationSemantics
		step := &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.value}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.input.Subject), Action: item.action}, {Subject: slot(item.input.Subject), Action: item.action}}}}
		if len(access.Dependencies)+len(access.Sources) != 0 {
			if !slices.Contains(request.GetTargetFeatures(), provider.PlanningLifecycleFeature) {
				return refusal(family, item.index, "Native access ordering requires planning.lifecycle"), nil
			}
			step.Operation.Phase = protocol.PlanningOperation_LIFECYCLE
			step.Metadata.Effects = append(step.Metadata.Effects, access.Effects...)
			contribution.Dependencies = append(contribution.Dependencies, access.Dependencies...)
			if len(access.Sources) != 0 {
				result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
			}
		}
		contribution.Steps = append(contribution.Steps, step)
		contribution.Dependencies = append(contribution.Dependencies, edges...)
		result.Changes[item.index] = &protocol.PlanningReceipt{Subject: proto.CloneOf(item.input.Subject), Kind: family.ChangeKind, Strategy: family.Strategy, Steps: []*protocol.StepIdentity{proto.CloneOf(id)}}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func refusal(family Family, index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: family.ChangeKind, Message: message, Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}
}
