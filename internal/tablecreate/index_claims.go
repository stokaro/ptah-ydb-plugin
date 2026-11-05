package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// indexFootprint returns the index a host step of table creates or drops,
// when the step reads table and changes one index of it: the footprint the
// host gives an index addition or removal on a table this provider owns.
func indexFootprint(step *protocol.CommonPlanningStep, table *protocol.ComparisonIdentity) (string, protocol.PlanningEffect_Action, bool) {
	effects := step.GetMetadata().GetEffects()
	if step.GetParent() == nil || provider.CompareIdentities(step.GetParent(), table) != 0 || step.GetAddedColumn() != nil || step.GetCommonObject() != nil || len(effects) != 2 ||
		effects[0].GetAction() != protocol.PlanningEffect_READ || provider.CompareIdentities(effects[0].GetSubject(), table) != 0 {
		return "", 0, false
	}
	subject := effects[1].GetSubject()
	if subject.GetKind() != "index" || subject.GetName().GetSource() == "" || subject.GetParent().GetNormalized() != table.GetName().GetNormalized() ||
		subject.GetSchema().GetNormalized() != table.GetSchema().GetNormalized() || subject.GetDatabase().GetNormalized() != table.GetDatabase().GetNormalized() {
		return "", 0, false
	}
	return subject.GetName().GetSource(), effects[1].GetAction(), true
}

// claimIndexSteps reads the host's steps that add a declared common index to
// an altered table or drop a held one, as index changes that replace them. An
// addition is compiled from the declaration with the rules a creation uses,
// so an index YDB could not create with the table is refused here as there.
// A removal drops the index the table holds under that name, as the native
// index read describes it. A non-empty reason refuses the request.
func claimIndexSteps(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep, held *native.IndexObservations,
	caps map[string]bool) ([]indexChange, string, error) {
	var changes []indexChange
	var declared *Ydb_Table.CreateTableRequest
	for _, step := range steps {
		if rename := step.GetRenamedIndex(); rename != nil && step.GetParent() != nil && provider.CompareIdentities(step.GetParent(), table.Subject) == 0 {
			change, reason := claimIndexRename(table, rename, held, caps)
			if reason != "" {
				return nil, reason, nil
			}
			change.source = step
			changes = append(changes, change)
			continue
		}
		name, action, shaped := indexFootprint(step, table.Subject)
		if !shaped {
			continue
		}
		if slices.ContainsFunc(changes, func(change indexChange) bool {
			return change.name() == name && (change.add != nil) == (action == protocol.PlanningEffect_CREATE)
		}) {
			return nil, fmt.Sprintf("The host changes index %q more than once", name), nil
		}
		switch action {
		case protocol.PlanningEffect_CREATE:
			if declared == nil {
				compiled, _, err := compile(owner, table.Desired, caps)
				if err != nil {
					return nil, err.Error(), nil
				}
				declared = compiled
			}
			at := slices.IndexFunc(declared.GetIndexes(), func(index *Ydb_Table.TableIndex) bool { return index.GetName() == name })
			if at < 0 {
				return nil, fmt.Sprintf("The host adds index %q, which the declaration does not state", name), nil
			}
			changes = append(changes, indexChange{add: indexIntentOf(declared.GetIndexes()[at]), source: step})
		case protocol.PlanningEffect_DROP:
			description := heldIndex(table.Current, held, name)
			if description == nil {
				return nil, fmt.Sprintf("The host drops index %q, which the table's index read does not hold", name), nil
			}
			changes = append(changes, indexChange{drop: description, source: step})
		default:
			return nil, fmt.Sprintf("The host changes index %q in a way this provider does not plan", name), nil
		}
	}
	// A changed index is dropped before it is added again under its name.
	slices.SortStableFunc(changes, func(a, b indexChange) int {
		return a.order() - b.order()
	})
	return changes, "", nil
}

// IndexRenameCapability is an index renamed in place, as the host's
// vocabulary spells it. YDB renames one with EnableMoveIndex on, its default
// on every line from 25.1 to 26.2.
const IndexRenameCapability = "index_rename"

// claimIndexRename reads the host's step that renames an index of table in
// place. The index keeps its definition and settings under the new name. A
// session reports index_rename only with EnableMoveIndex on, and YDB refuses
// the rename without it (`Move index is not supported yet`), so a rename on a
// session without the capability is refused before dispatch. A non-empty
// reason refuses the request.
func claimIndexRename(table *protocol.PlanningTable, rename *protocol.CommonIndexRename, held *native.IndexObservations, caps map[string]bool) (indexChange, string) {
	if !caps[IndexRenameCapability] {
		return indexChange{}, fmt.Sprintf("The host renames index %q to %q, and this cluster renames no index in place: EnableMoveIndex is off", rename.GetFrom(), rename.GetTo())
	}
	description := heldIndex(table.Current, held, rename.GetFrom())
	if description == nil {
		return indexChange{}, fmt.Sprintf("The host renames index %q, which the table's index read does not hold", rename.GetFrom())
	}
	return indexChange{rename: &indexRename{held: description, to: rename.GetTo()}}, ""
}

// heldIndex is the index named name the table holds: as the native index read
// describes it, or, for an index the common model describes and the native
// read leaves to the host, as the host's read of the table describes it.
func heldIndex(current *protocol.ObservedTable, held *native.IndexObservations, name string) *native.TableIndexDescription {
	if at := slices.IndexFunc(held.GetIndexes(), func(index *native.ObservedIndex) bool { return index.GetDescription().GetName() == name }); at >= 0 {
		return proto.CloneOf(held.GetIndexes()[at].GetDescription())
	}
	at := slices.IndexFunc(current.GetIndexes(), func(index *protocol.ObservedIndex) bool { return index.GetName() == name })
	if at < 0 {
		return nil
	}
	index := current.GetIndexes()[at]
	result := &native.TableIndexDescription{Name: index.GetName(), IndexColumns: slices.Clone(index.GetColumns()), DataColumns: slices.Clone(index.GetIncludeColumns())}
	switch {
	case index.GetUnique():
		result.Type = &native.TableIndexDescription_GlobalUniqueIndex{GlobalUniqueIndex: &native.GlobalUniqueIndex{}}
	case index.GetMethod() == "async":
		result.Type = &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{}}
	default:
		result.Type = &native.TableIndexDescription_GlobalIndex{GlobalIndex: &native.GlobalIndex{}}
	}
	return result
}

// order is where a change runs among a table's index changes: an addition,
// 1, after any other change, 0.
func (c indexChange) order() int {
	if c.add != nil {
		return 1
	}
	return 0
}

// indexIntentOf is the native specification of a compiled global index.
func indexIntentOf(index *Ydb_Table.TableIndex) *native.IndexIntent {
	result := &native.IndexIntent{Name: index.GetName(), IndexColumns: slices.Clone(index.GetIndexColumns()), DataColumns: slices.Clone(index.GetDataColumns())}
	switch index.GetType().(type) {
	case *Ydb_Table.TableIndex_GlobalAsyncIndex:
		result.Type = &native.IndexIntent_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{}}
	case *Ydb_Table.TableIndex_GlobalUniqueIndex:
		result.Type = &native.IndexIntent_GlobalUniqueIndex{GlobalUniqueIndex: &native.GlobalUniqueIndex{}}
	default:
		result.Type = &native.IndexIntent_GlobalIndex{GlobalIndex: &native.GlobalIndex{}}
	}
	return result
}

// settingsAfterAdditions orders the addition of an index, or its rename,
// before the change that gives it its declared settings, which needs the index
// to exist under the name the change names.
func settingsAfterAdditions(steps []*indexStep, settings []*indexSettingsStep) []*protocol.PlanningDependency {
	var result []*protocol.PlanningDependency
	for _, add := range steps {
		if add.change.add == nil && add.change.rename == nil {
			continue
		}
		for _, change := range settings {
			if change.change.held.GetName() == add.change.name() {
				result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(add.id), After: proto.CloneOf(change.id)})
			}
		}
	}
	return result
}

// indexReplacements orders the removal of an index before the addition of an
// index under the same name, which replaces it.
func indexReplacements(steps []*indexStep) []*protocol.PlanningDependency {
	var result []*protocol.PlanningDependency
	for _, drop := range steps {
		if drop.change.drop == nil {
			continue
		}
		for _, add := range steps {
			if add.change.add != nil && add.change.name() == drop.change.name() {
				result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(drop.id), After: proto.CloneOf(add.id)})
			}
		}
	}
	return result
}
