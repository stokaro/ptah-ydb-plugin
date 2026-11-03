package tablecreate

import (
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
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
			at := slices.IndexFunc(held.GetIndexes(), func(index *native.ObservedIndex) bool { return index.GetDescription().GetName() == name })
			if at < 0 {
				return nil, fmt.Sprintf("The host drops index %q, which the table's index read does not hold", name), nil
			}
			changes = append(changes, indexChange{drop: proto.CloneOf(held.GetIndexes()[at].GetDescription()), source: step})
		default:
			return nil, fmt.Sprintf("The host changes index %q in a way this provider does not plan", name), nil
		}
	}
	// A changed index is dropped before it is added again under its name.
	slices.SortStableFunc(changes, func(a, b indexChange) int {
		return boolOrder(a.add != nil) - boolOrder(b.add != nil)
	})
	return changes, "", nil
}

func boolOrder(value bool) int {
	if value {
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
