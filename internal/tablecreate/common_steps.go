package tablecreate

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// commonObjectDependencies orders the host's view and comment steps against
// the steps this service contributes, as the host's embedded YDB planner
// orders them.
//
// Views go around every table step: YDB checks a view's query against the
// schema when the view is created and never again, and records no
// dependency on a view or on the table it reads, so a view is dropped before
// any table changes and created after every table is in place. A comment is an
// attribute of its table, or of a view, and YDB sets one only on an object
// that exists: a comment on a table, its column or its index comes after every
// step this service makes on that table.
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
				result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(step.Id), After: proto.CloneOf(host.Id)})
			case *protocol.CommonObjectChange_SetComment:
				if schema, table, onTable := commentedTable(operation.SetComment.GetObject()); onTable && names(step, schema, table) {
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
