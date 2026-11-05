package objectplan

import (
	"fmt"
	"slices"
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

// sibling is another change of the same planning request: its subject, what
// its step does to it, and the step's identity.
type sibling struct {
	subject *protocol.ComparisonIdentity
	action  protocol.PlanningEffect_Action
	id      *protocol.StepIdentity
}

// dependencies orders the step id of item against the common steps, the
// captured tables and the siblings of request that act on its path or on a
// path above or below it. Without siblings, another change of request at a
// path above or below is refused.
func dependencies(request *protocol.PlanFeaturesRequest, family Family, item change, id *protocol.StepIdentity, siblings []sibling) ([]*protocol.PlanningDependency, error) {
	occupied, err := commonOccupants(request, family, item)
	if err != nil {
		return nil, err
	}
	edges, err := occupied.order(family, item, id)
	if err != nil {
		return nil, err
	}
	if err = checkPathTables(request, family, item, occupied); err != nil {
		return nil, err
	}
	ordered, err := checkPathChanges(request, family, item, id, siblings)
	if err != nil {
		return nil, err
	}
	return append(edges, ordered...), nil
}

// occupants are the common steps that act on the path of one change: each
// step and action once, in the order the steps report them, and handoff, the
// step that creates or drops the path's occupant.
//
// YDB needs every path above an object to be a directory. A creation follows
// the common steps in after, which drop the occupant of a path above it, and
// a drop precedes those in before, which create an object below its path.
// freed and filled hold those paths, and kept the paths above a creation that
// a common step reads or alters, which a drop there must free.
type occupants struct {
	uses    []use
	handoff *protocol.StepIdentity
	after   []*protocol.StepIdentity
	before  []*protocol.StepIdentity
	freed   map[string]bool
	filled  map[string]bool
	kept    []string
}

// commonOccupants reads the effects of the common steps of request against
// the path of item. A step with no effects is skipped when it is an ordering
// marker and refused otherwise.
func commonOccupants(request *protocol.PlanFeaturesRequest, family Family, item change) (occupants, error) {
	occupied := occupants{freed: make(map[string]bool), filled: make(map[string]bool)}
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
	for _, kept := range occupied.kept {
		if !occupied.freed[kept] {
			return occupants{}, errOverlap
		}
	}
	return occupied, nil
}

// visit reads one effect of the common step step against the path of item.
// An effect at the path is noted once; a creation or a drop there becomes
// the handoff, which must be the expected action of a creation or a drop and
// the only one. An effect at a path above or below the item's is ordered by
// above and below.
func (o *occupants) visit(family Family, item change, step *protocol.StepIdentity, effect *protocol.PlanningEffect, expected protocol.PlanningEffect_Action) error {
	// A column or an index lives in its table and occupies no path of
	// its own; the table's effect beside it carries the path.
	if effect.GetSubject().GetParent().GetNormalized() != "" {
		return nil
	}
	// The access list of a table or a view, which a grant or a revoke
	// on it changes, and a comment are resources of their own, named
	// by the object with the kind access or comment. They occupy no
	// path; the step's read of the object beside them carries it.
	if kind := effect.GetSubject().GetKind(); kind == "access" || kind == "comment" {
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
	switch {
	case ancestor(effect.Subject, item.input.Subject):
		return o.above(item, step, effect)
	case ancestor(item.input.Subject, effect.Subject):
		return o.below(item, step, effect)
	}
	return nil
}

// errOverlap refuses a path above or below an object's that no order of the
// plan's steps can leave a directory.
var errOverlap = fmt.Errorf("overlapping parent and descendant paths require a captured directory lifecycle")

// above reads an effect of the common step step at a path above item's. A
// creation follows a drop there; a read or an alteration there is left to
// that drop, which comes after it, and needs one. A drop below a path the
// plan creates an object at is refused, since YDB keeps the directory. Any
// other pair is refused.
func (o *occupants) above(item change, step *protocol.StepIdentity, effect *protocol.PlanningEffect) error {
	at := pathKey(effect.Subject)
	switch {
	case item.action == protocol.PlanningEffect_CREATE && effect.Action == protocol.PlanningEffect_DROP:
		o.after = append(o.after, step)
		o.freed[at] = true
	case item.action == protocol.PlanningEffect_CREATE && (effect.Action == protocol.PlanningEffect_READ || effect.Action == protocol.PlanningEffect_ALTER):
		o.kept = append(o.kept, at)
	case item.action == protocol.PlanningEffect_DROP && effect.Action == protocol.PlanningEffect_CREATE:
		return directoryKept(pathKey(item.input.Subject), at)
	default:
		return errOverlap
	}
	return nil
}

// below reads an effect of the common step step at a path below item's. A
// drop precedes a creation there. A creation where the plan drops an object
// below is refused, since YDB keeps the directory. Any other pair is refused.
func (o *occupants) below(item change, step *protocol.StepIdentity, effect *protocol.PlanningEffect) error {
	at := pathKey(effect.Subject)
	switch {
	case item.action == protocol.PlanningEffect_DROP && effect.Action == protocol.PlanningEffect_CREATE:
		o.before = append(o.before, step)
		o.filled[at] = true
	case item.action == protocol.PlanningEffect_CREATE && effect.Action == protocol.PlanningEffect_DROP:
		return directoryKept(at, pathKey(item.input.Subject))
	default:
		return errOverlap
	}
	return nil
}

// directoryKept refuses a plan that drops the object at dropped and creates
// one at directory, the path of a directory above it. YDB keeps a directory
// after its last object is dropped, and YQL has no statement that removes
// one, so no order of the plan's statements can apply it.
func directoryKept(dropped, directory string) error {
	return fmt.Errorf("this plan drops %s and creates an object at %s, which is a directory in the database. "+
		"YDB keeps a directory after its last object is dropped, and YQL has no statement that removes one: "+
		"drop the objects below %s and remove the directory first (for example with `ydb scheme rmdir`), "+
		"or declare the object at another path", dropped, directory, directory)
}

// pathKey writes the scheme path subject occupies relative to the database,
// as YQL names the object there.
func pathKey(subject *protocol.ComparisonIdentity) string {
	if subject.GetSchema().GetNormalized() == "" {
		return subject.GetName().GetNormalized()
	}
	return subject.GetSchema().GetNormalized() + "/" + subject.GetName().GetNormalized()
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
	for _, step := range o.after {
		edges.add(step, id)
	}
	for _, step := range o.before {
		edges.add(id, step)
	}
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

// checkPathTables refuses a captured table of request at the item's path
// whose lifecycle does not hand the path over: a creation needs the table
// dropped, a drop needs it created, both need the handoff, and an alteration
// takes none. A table above a creation must be dropped by a common step the
// creation follows, and one below a drop created by a common step the drop
// precedes; a table dropped below a creation, or created above a drop, keeps
// a directory there and is refused, as is any other overlap.
func checkPathTables(request *protocol.PlanFeaturesRequest, family Family, item change, occupied occupants) error {
	handoff := occupied.handoff
	for _, table := range request.Tables {
		if err := validatePath(family, table.Subject, item.input.Subject); err != nil {
			return err
		}
		if err := checkTableAboveOrBelow(family, item, table, occupied); err != nil {
			return err
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

// checkTableAboveOrBelow refuses a captured table above item's path that is
// not dropped by a common step the item's creation follows, and one below that
// is not created by a common step the item's drop precedes. A table created
// above a drop, or dropped below a creation, keeps a directory there and is
// refused with that reason.
func checkTableAboveOrBelow(family Family, item change, table *protocol.PlanningTable, occupied occupants) error {
	at := pathKey(table.Subject)
	switch {
	case ancestor(table.Subject, item.input.Subject) && item.action == protocol.PlanningEffect_DROP && table.Action == protocol.PlanningTable_CREATE:
		return directoryKept(pathKey(item.input.Subject), at)
	case ancestor(item.input.Subject, table.Subject) && item.action == protocol.PlanningEffect_CREATE && table.Action == protocol.PlanningTable_DROP:
		return directoryKept(at, pathKey(item.input.Subject))
	case ancestor(table.Subject, item.input.Subject) && (table.Action != protocol.PlanningTable_DROP || !occupied.freed[at]),
		ancestor(item.input.Subject, table.Subject) && (table.Action != protocol.PlanningTable_CREATE || !occupied.filled[at]):
		return fmt.Errorf("captured table overlaps the directory path of a %s", family.Noun)
	}
	return nil
}

// checkPathChanges orders the step id of item against another change of
// request at a path above or below its own: a creation follows the sibling
// that drops the object above it, and the creation of a sibling below a drop
// orders itself the same way. A drop below a creation keeps a directory there
// and is refused. Any other pair, and any change of a request planned without
// siblings, is refused: such a transition needs a separately captured
// directory lifecycle.
func checkPathChanges(request *protocol.PlanFeaturesRequest, family Family, item change, id *protocol.StepIdentity, siblings []sibling) ([]*protocol.PlanningDependency, error) {
	var edges edgeList
	for _, other := range request.Changes {
		if provider.CompareIdentities(other.Subject, item.input.Subject) == 0 {
			continue
		}
		above, below := ancestor(other.Subject, item.input.Subject), ancestor(item.input.Subject, other.Subject)
		if !above && !below {
			continue
		}
		index := slices.IndexFunc(siblings, func(s sibling) bool { return provider.CompareIdentities(s.subject, other.Subject) == 0 })
		if index < 0 {
			return nil, fmt.Errorf("%s changes contain overlapping object and directory paths", family.Noun)
		}
		known := siblings[index]
		switch {
		case above && known.action == protocol.PlanningEffect_DROP && item.action == protocol.PlanningEffect_CREATE:
			edges.add(known.id, id)
		case below && item.action == protocol.PlanningEffect_DROP && known.action == protocol.PlanningEffect_CREATE:
			// The creation below orders itself after this drop.
		case above && known.action == protocol.PlanningEffect_CREATE && item.action == protocol.PlanningEffect_DROP:
			return nil, directoryKept(pathKey(item.input.Subject), pathKey(other.Subject))
		case below && item.action == protocol.PlanningEffect_CREATE && known.action == protocol.PlanningEffect_DROP:
			return nil, directoryKept(pathKey(other.Subject), pathKey(item.input.Subject))
		default:
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
// common steps and the captured tables of request that occupy its path or a
// path above or below it, as Plan orders the step of a change. A family that
// plans more than one step for a change asks for each. Another change of
// request at a path above or below subject is refused.
func PathDependencies(request *protocol.PlanFeaturesRequest, family Family, subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
	return dependencies(request, family, change{input: &protocol.PlanningChange{Subject: subject}, action: action}, id, nil)
}

// Slot is the scheme path subject occupies, the identity a step's effect
// names beside the object's own.
func Slot(subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity { return slot(subject) }
