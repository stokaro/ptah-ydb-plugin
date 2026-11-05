package objectplan

import (
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/schema"
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

// isSchemeSubject reports whether subject occupies a scheme path of its own:
// a path, a table, a view or a native object. A view the host creates or
// drops is an entry of its directory, as a table is.
func isSchemeSubject(subject *protocol.ComparisonIdentity) bool {
	return subject.GetKind() == PathKind || subject.GetKind() == "table" || subject.GetKind() == "view" || schema.IsNativeObject(subject.GetKind())
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
	occupied, err := commonOccupants(request, family, item)
	if err != nil {
		return nil, err
	}
	edges, err := occupied.order(family, item, id)
	if err != nil {
		return nil, err
	}
	if err = checkPathTables(request, family, item, occupied.handoff); err != nil {
		return nil, err
	}
	if err = checkPathChanges(request, family, item); err != nil {
		return nil, err
	}
	return edges, nil
}

// occupants are the common steps that act on the path of one change: each
// step and action once, in the order the steps report them, and handoff, the
// step that creates or drops the path's occupant.
type occupants struct {
	uses    []use
	handoff *protocol.StepIdentity
}

// commonOccupants reads the effects of the common steps of request against
// the path of item. A step with no effects is skipped when it is an ordering
// marker and refused otherwise.
func commonOccupants(request *protocol.PlanFeaturesRequest, family Family, item change) (occupants, error) {
	var occupied occupants
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
			return occupants{}, fmt.Errorf("a common step has no established object footprint")
		}
		for _, effect := range common.Metadata.Effects {
			if err := occupied.visit(family, item, common.Id, effect, expected); err != nil {
				return occupants{}, err
			}
		}
	}
	return occupied, nil
}

// visit reads one effect of the common step step against the path of item.
// An effect at the path is noted once; a creation or a drop there becomes
// the handoff, which must be the expected action of a creation or a drop and
// the only one. A path above or below the item's is refused.
func (o *occupants) visit(family Family, item change, step *protocol.StepIdentity, effect *protocol.PlanningEffect, expected protocol.PlanningEffect_Action) error {
	// A column or an index lives in its table and occupies no path of
	// its own; the table's effect beside it carries the path.
	if effect.GetSubject().GetParent().GetNormalized() != "" {
		return nil
	}
	// A directory effect needs its own path projection. Until that
	// contract exists, it cannot establish a disjoint footprint.
	if !isSchemeSubject(effect.Subject) {
		return fmt.Errorf("common step has an unsupported path footprint")
	}
	if err := validatePath(family, effect.Subject, item.input.Subject); err != nil {
		return err
	}
	if samePath(effect.Subject, item.input.Subject) {
		// A common step may report both a table and its shared physical slot.
		if o.holds(step, effect.Action) {
			return nil
		}
		o.uses = append(o.uses, use{step, effect.Action})
		if effect.Action == protocol.PlanningEffect_CREATE || effect.Action == protocol.PlanningEffect_DROP {
			if item.action == protocol.PlanningEffect_ALTER || effect.Action != expected || o.handoff != nil {
				return fmt.Errorf("%s operation conflicts with a common occupant at its path", family.Noun)
			}
			o.handoff = step
		}
	}
	if ancestor(effect.Subject, item.input.Subject) || ancestor(item.input.Subject, effect.Subject) {
		return fmt.Errorf("overlapping parent and descendant paths require a captured directory lifecycle")
	}
	return nil
}

// holds reports whether step is already noted with action.
func (o *occupants) holds(step *protocol.StepIdentity, action protocol.PlanningEffect_Action) bool {
	for _, u := range o.uses {
		if proto.Equal(u.id, step) && u.action == action {
			return true
		}
	}
	return false
}

// order returns the dependencies that place the step id of item against the
// occupants of its path: a creation runs after the handoff, which runs after
// every other occupant, and a drop runs the other way round. Occupants with
// no handoff, or beside an alteration, are refused as unordered.
func (o occupants) order(family Family, item change, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
	var edges edgeList
	if len(o.uses) == 0 {
		return edges, nil
	}
	if o.handoff == nil || item.action == protocol.PlanningEffect_ALTER {
		return nil, fmt.Errorf("%s operation has an unordered path occupant", family.Noun)
	}
	if item.action == protocol.PlanningEffect_CREATE {
		edges.add(o.handoff, id)
	} else {
		edges.add(id, o.handoff)
	}
	for _, u := range o.uses {
		if proto.Equal(u.id, o.handoff) {
			continue
		}
		if item.action == protocol.PlanningEffect_CREATE {
			edges.add(u.id, o.handoff)
		} else {
			edges.add(o.handoff, u.id)
		}
	}
	return edges, nil
}

// edgeList holds planning dependencies, each pair of steps once, in the
// order the pairs were first added.
type edgeList []*protocol.PlanningDependency

// add appends the dependency of after on before unless the list holds it.
func (l *edgeList) add(before, after *protocol.StepIdentity) {
	for _, edge := range *l {
		if proto.Equal(edge.Before, before) && proto.Equal(edge.After, after) {
			return
		}
	}
	*l = append(*l, &protocol.PlanningDependency{Before: proto.CloneOf(before), After: proto.CloneOf(after)})
}

// checkPathTables refuses a captured table of request whose path overlaps the
// directory path of item, and one at the item's path whose lifecycle does not
// hand the path over: a creation needs the table dropped, a drop needs it
// created, both need the handoff, and an alteration takes none.
func checkPathTables(request *protocol.PlanFeaturesRequest, family Family, item change, handoff *protocol.StepIdentity) error {
	for _, table := range request.Tables {
		if err := validatePath(family, table.Subject, item.input.Subject); err != nil {
			return err
		}
		if ancestor(table.Subject, item.input.Subject) || ancestor(item.input.Subject, table.Subject) {
			return fmt.Errorf("captured table overlaps the directory path of a %s", family.Noun)
		}
		if !samePath(table.Subject, item.input.Subject) {
			continue
		}
		if handoff == nil ||
			(item.action == protocol.PlanningEffect_CREATE && table.Action != protocol.PlanningTable_DROP) ||
			(item.action == protocol.PlanningEffect_DROP && table.Action != protocol.PlanningTable_CREATE) ||
			item.action == protocol.PlanningEffect_ALTER {
			return fmt.Errorf("captured table lifecycle conflicts with the path of a %s", family.Noun)
		}
	}
	return nil
}

// checkPathChanges refuses another change of request whose subject is the
// parent directory of item's, or the other way round. Native objects cannot
// become each other's parent directories in one batch; such a transition
// needs a separately captured directory lifecycle.
func checkPathChanges(request *protocol.PlanFeaturesRequest, family Family, item change) error {
	for _, other := range request.Changes {
		if provider.CompareIdentities(other.Subject, item.input.Subject) == 0 {
			continue
		}
		if ancestor(other.Subject, item.input.Subject) || ancestor(item.input.Subject, other.Subject) {
			return fmt.Errorf("%s changes contain overlapping object and directory paths", family.Noun)
		}
	}
	return nil
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
