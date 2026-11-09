package tablecreate

import (
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// commonObjectDependencies orders the host's view and comment steps against
// the steps this service contributes, as the host's embedded YDB planner
// orders them.
//
// Views go around the table steps: YDB checks a view's query against the
// schema when the view is created and never again, and records no
// dependency on a view or on the table it reads, so a view is dropped before
// any table changes and created once every table is in place. A comment is an
// attribute of its table, or of a view, and YDB sets one only on an object
// that exists: a comment on a table, its column or its index comes after every
// step this service makes on that table.
//
// A step that only removes is the exception for both. The host places a
// removal, such as a column's, after its views and comments, so a view or a
// comment ordered after it closes a cycle; and neither needs it, since what a
// removal drops is no longer declared, so no view the plan creates reads it and
// no comment the plan sets names it.
//
// A comment on the table or on one of its columns does not wait for a step
// that changes only an index of the table either. The host places the comment
// of a column it adds right after the column, before the index additions, so
// the edge closes a cycle; and the comment does not need it, since the step
// changes nothing the comment names. A comment on an index still waits.
func commonObjectDependencies(common []*protocol.CommonPlanningStep, steps []*protocol.PlanningStep) []*protocol.PlanningDependency {
	var result []*protocol.PlanningDependency
	for _, host := range common {
		change := host.GetCommonObject()
		if change == nil {
			continue
		}
		for _, step := range steps {
			switch operation := change.GetOperation().(type) {
			case *protocol.CommonObjectChange_DropView:
				result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(host.Id), After: proto.CloneOf(step.Id)})
			case *protocol.CommonObjectChange_CreateView:
				if !removalOnly(step) {
					result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(step.Id), After: proto.CloneOf(host.Id)})
				}
			case *protocol.CommonObjectChange_SetComment:
				object := operation.SetComment.GetObject()
				schema, table, onTable := commentedTable(object)
				if onTable && names(step, schema, table) && !removalOnly(step) && (object.GetKind() == "index" || !indexOnly(step)) {
					result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(step.Id), After: proto.CloneOf(host.Id)})
				}
			}
		}
	}
	return result
}

// commentedTable is the table that keeps a comment: the table itself, or the
// table of a column or an index.
func commentedTable(object *protocol.Reference) (schema, table string, onTable bool) {
	switch object.GetKind() {
	case "table":
		return object.GetSchema(), object.GetName(), true
	case "column", "index":
		if len(object.GetPath()) == 1 {
			return object.GetSchema(), object.GetPath()[0], true
		}
	}
	return "", "", false
}

// names reports a step with an effect on the table schema and table name, or
// on a part of it such as its column families or its TTL.
func names(step *protocol.PlanningStep, schema, table string) bool {
	for _, effect := range step.GetMetadata().GetEffects() {
		subject := effect.GetSubject()
		if subject.GetSchema().GetNormalized() == schema && subject.GetName().GetNormalized() == table {
			return true
		}
	}
	return false
}

// removalOnly reports a step that only removes: each of its effects reads or
// drops, and at least one drops, as a column's removal reads its table and
// drops the column.
func removalOnly(step *protocol.PlanningStep) bool {
	dropped := false
	for _, effect := range step.GetMetadata().GetEffects() {
		switch effect.GetAction() {
		case protocol.PlanningEffect_DROP:
			dropped = true
		case protocol.PlanningEffect_READ:
		default:
			return false
		}
	}
	return dropped
}

// indexOnly reports a step that changes only indexes of its table: each of its
// effects reads or names an index, and at least one names an index, as an
// index's addition reads its table and creates the index.
func indexOnly(step *protocol.PlanningStep) bool {
	changed := false
	for _, effect := range step.GetMetadata().GetEffects() {
		switch {
		case effect.GetSubject().GetKind() == "index":
			changed = true
		case effect.GetAction() != protocol.PlanningEffect_READ:
			return false
		}
	}
	return changed
}
