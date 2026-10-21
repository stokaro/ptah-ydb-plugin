package coordinationplan

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func slot(subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	value := proto.CloneOf(subject)
	value.Kind = PathKind
	return value
}
func isSchemeSubject(subject *protocol.ComparisonIdentity) bool {
	return subject.GetKind() == PathKind || subject.GetKind() == "table" || schema.IsNativeObject(subject.GetKind())
}
func samePath(a, b *protocol.ComparisonIdentity) bool {
	return isSchemeSubject(a) && isSchemeSubject(b) && provider.CompareIdentities(slot(a), slot(b)) == 0
}
func ancestor(parent, child *protocol.ComparisonIdentity) bool {
	if !isSchemeSubject(parent) || parent.GetDatabase().GetNormalized() != child.GetDatabase().GetNormalized() || parent.GetCatalog().GetNormalized() != child.GetCatalog().GetNormalized() || len(parent.GetPath())+len(parent.GetSignature()) != 0 || parent.GetParent().GetNormalized() != "" {
		return false
	}
	directory := parent.GetName().GetNormalized()
	if parent.GetSchema().GetNormalized() != "" {
		directory = parent.GetSchema().GetNormalized() + "/" + directory
	}
	namespace := child.GetSchema().GetNormalized()
	return namespace == directory || strings.HasPrefix(namespace, directory+"/")
}

type use struct {
	id     *protocol.StepIdentity
	action protocol.PlanningEffect_Action
}

func dependencies(request *protocol.PlanFeaturesRequest, item change, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
	var uses []use
	var edges []*protocol.PlanningDependency
	var handoff *protocol.StepIdentity
	add := func(before, after *protocol.StepIdentity) {
		for _, edge := range edges {
			if proto.Equal(edge.Before, before) && proto.Equal(edge.After, after) {
				return
			}
		}
		edges = append(edges, &protocol.PlanningDependency{Before: proto.CloneOf(before), After: proto.CloneOf(after)})
	}
	expected := protocol.PlanningEffect_CREATE
	if item.action == protocol.PlanningEffect_CREATE {
		expected = protocol.PlanningEffect_DROP
	}
	for _, common := range request.CommonSteps {
		if len(common.GetMetadata().GetEffects()) == 0 {
			return nil, fmt.Errorf("a common step has no established object footprint")
		}
		for _, effect := range common.Metadata.Effects {
			// A directory or child-object effect needs its own path projection.
			// Until that contract exists, it cannot establish a disjoint footprint.
			if !isSchemeSubject(effect.Subject) {
				return nil, fmt.Errorf("common step has an unsupported path footprint")
			}
			if err := validatePath(effect.Subject, item.input.Subject); err != nil {
				return nil, err
			}
			if samePath(effect.Subject, item.input.Subject) {
				// A common step may report both a table and its shared physical slot.
				duplicate := false
				for _, u := range uses {
					if proto.Equal(u.id, common.Id) && u.action == effect.Action {
						duplicate = true
					}
				}
				if duplicate {
					continue
				}
				uses = append(uses, use{common.Id, effect.Action})
				if effect.Action == protocol.PlanningEffect_CREATE || effect.Action == protocol.PlanningEffect_DROP {
					if item.action == protocol.PlanningEffect_ALTER || effect.Action != expected || handoff != nil {
						return nil, fmt.Errorf("coordination operation conflicts with a common occupant at its path")
					}
					handoff = common.Id
				}
			}
			if ancestor(effect.Subject, item.input.Subject) || ancestor(item.input.Subject, effect.Subject) {
				return nil, fmt.Errorf("overlapping parent and descendant paths require a captured directory lifecycle")
			}
		}
	}
	if len(uses) > 0 {
		if handoff == nil || item.action == protocol.PlanningEffect_ALTER {
			return nil, fmt.Errorf("coordination operation has an unordered path occupant")
		}
		if item.action == protocol.PlanningEffect_CREATE {
			add(handoff, id)
		} else {
			add(id, handoff)
		}
		for _, u := range uses {
			if proto.Equal(u.id, handoff) {
				continue
			}
			if item.action == protocol.PlanningEffect_CREATE {
				add(u.id, handoff)
			} else {
				add(handoff, u.id)
			}
		}
	}
	for _, table := range request.Tables {
		if err := validatePath(table.Subject, item.input.Subject); err != nil {
			return nil, err
		}
		if ancestor(table.Subject, item.input.Subject) || ancestor(item.input.Subject, table.Subject) {
			return nil, fmt.Errorf("captured table overlaps a coordination directory path")
		}
		if !samePath(table.Subject, item.input.Subject) {
			continue
		}
		if handoff == nil || (item.action == protocol.PlanningEffect_CREATE && table.Action != protocol.PlanningTable_DROP) || (item.action == protocol.PlanningEffect_DROP && table.Action != protocol.PlanningTable_CREATE) || item.action == protocol.PlanningEffect_ALTER {
			return nil, fmt.Errorf("captured table lifecycle conflicts with the coordination path")
		}
	}
	// Native objects cannot become each other's parent directories in one batch.
	// Such a transition needs a separately captured directory lifecycle.
	for _, other := range request.Changes {
		if provider.CompareIdentities(other.Subject, item.input.Subject) == 0 {
			continue
		}
		if ancestor(other.Subject, item.input.Subject) || ancestor(item.input.Subject, other.Subject) {
			return nil, fmt.Errorf("coordination changes contain overlapping object and directory paths")
		}
	}
	return edges, nil
}

// An omitted database is an offline binding, not evidence of a separate path.
func validatePath(subject, bound *protocol.ComparisonIdentity) error {
	identity := proto.CloneOf(subject)
	identity.Kind = schema.CoordinationProperties
	if _, err := schema.CoordinationBoundOwner(identity); err != nil {
		return fmt.Errorf("common context has an invalid YDB path identity: %w", err)
	}
	if (subject.GetDatabase().GetNormalized() == "") != (bound.GetDatabase().GetNormalized() == "") {
		return fmt.Errorf("common context has an unresolved database binding")
	}
	return nil
}
