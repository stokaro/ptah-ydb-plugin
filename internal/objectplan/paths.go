package objectplan

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// slot is the scheme path a subject occupies, named as the host names a path
// of the session's database: without the database. A plan runs in one
// database, and the host names a declared object or a table without it while
// a read object keeps the database its inspection named.
func slot(subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	value := proto.CloneOf(subject)
	value.Kind = PathKind
	value.Database = &protocol.ComparisonPart{}
	return value
}
func isSchemeSubject(subject *protocol.ComparisonIdentity) bool {
	return subject.GetKind() == PathKind || subject.GetKind() == "table" || schema.IsNativeObject(subject.GetKind())
}
func samePath(a, b *protocol.ComparisonIdentity) bool {
	return isSchemeSubject(a) && isSchemeSubject(b) && provider.CompareIdentities(slot(a), slot(b)) == 0
}
func ancestor(parent, child *protocol.ComparisonIdentity) bool {
	if !isSchemeSubject(parent) || parent.GetCatalog().GetNormalized() != child.GetCatalog().GetNormalized() || len(parent.GetPath())+len(parent.GetSignature()) != 0 || parent.GetParent().GetNormalized() != "" {
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

func dependencies(request *protocol.PlanFeaturesRequest, family Family, item change, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
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
			// An ordering marker such as common/feature-creations belongs to
			// no object and changes nothing; the host places the step between
			// its markers.
			if common.Parent == nil && common.AddedColumn == nil {
				continue
			}
			return nil, fmt.Errorf("a common step has no established object footprint")
		}
		for _, effect := range common.Metadata.Effects {
			// A column or an index lives in its table and occupies no path of
			// its own; the table's effect beside it carries the path.
			if effect.GetSubject().GetParent().GetNormalized() != "" {
				continue
			}
			// A directory effect needs its own path projection. Until that
			// contract exists, it cannot establish a disjoint footprint.
			if !isSchemeSubject(effect.Subject) {
				return nil, fmt.Errorf("common step has an unsupported path footprint")
			}
			if err := validatePath(family, effect.Subject, item.input.Subject); err != nil {
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
						return nil, fmt.Errorf("%s operation conflicts with a common occupant at its path", family.Noun)
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
			return nil, fmt.Errorf("%s operation has an unordered path occupant", family.Noun)
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
		if err := validatePath(family, table.Subject, item.input.Subject); err != nil {
			return nil, err
		}
		if ancestor(table.Subject, item.input.Subject) || ancestor(item.input.Subject, table.Subject) {
			return nil, fmt.Errorf("captured table overlaps the directory path of a %s", family.Noun)
		}
		if !samePath(table.Subject, item.input.Subject) {
			continue
		}
		if handoff == nil || (item.action == protocol.PlanningEffect_CREATE && table.Action != protocol.PlanningTable_DROP) || (item.action == protocol.PlanningEffect_DROP && table.Action != protocol.PlanningTable_CREATE) || item.action == protocol.PlanningEffect_ALTER {
			return nil, fmt.Errorf("captured table lifecycle conflicts with the path of a %s", family.Noun)
		}
	}
	// Native objects cannot become each other's parent directories in one batch.
	// Such a transition needs a separately captured directory lifecycle.
	for _, other := range request.Changes {
		if provider.CompareIdentities(other.Subject, item.input.Subject) == 0 {
			continue
		}
		if ancestor(other.Subject, item.input.Subject) || ancestor(item.input.Subject, other.Subject) {
			return nil, fmt.Errorf("%s changes contain overlapping object and directory paths", family.Noun)
		}
	}
	return edges, nil
}

// An omitted database is the session's, not evidence of a separate path; two
// databases that are both named must be one.
func validatePath(family Family, subject, bound *protocol.ComparisonIdentity) error {
	identity := proto.CloneOf(subject)
	identity.Kind = family.Kind
	if _, err := family.Owner(identity); err != nil {
		return fmt.Errorf("common context has an invalid YDB path identity: %w", err)
	}
	a, b := subject.GetDatabase().GetNormalized(), bound.GetDatabase().GetNormalized()
	if a != "" && b != "" && a != b {
		return fmt.Errorf("common context names another database")
	}
	return nil
}

// PathDependencies orders one step that acts on subject with action, a
// creation, an alteration or a drop of an object of family, against the
// common steps and the captured tables of request that occupy its path, as
// Plan orders the step of a change. A family that plans more than one step
// for a change asks for each.
func PathDependencies(request *protocol.PlanFeaturesRequest, family Family, subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
	return dependencies(request, family, change{input: &protocol.PlanningChange{Subject: subject}, action: action}, id)
}

// Slot is the scheme path subject occupies, the identity a step's effect
// names beside the object's own.
func Slot(subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity { return slot(subject) }
