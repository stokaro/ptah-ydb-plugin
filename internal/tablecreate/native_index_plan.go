package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// nativeIndexInput is what the vector and full-text index rule reads: the
// index sets of both sides, as for planIndexes, the declared columns with
// their YDB types and the table's key, and the target's capabilities.
type nativeIndexInput struct {
	managed      bool
	desired      *native.IndexSetIntent
	held         *native.IndexObservations
	columns      nativeIndexColumns
	capabilities map[string]bool
}

// nativeIndexNames are the names of the indexes either side holds as a vector
// or full-text index.
func nativeIndexNames(desired *native.IndexSetIntent, held *native.IndexObservations) map[string]bool {
	names := make(map[string]bool)
	for _, declaration := range desired.GetIndexes() {
		if nativeOnlyIndex(declaration.GetSpecification()) {
			names[declaration.GetSpecification().GetName()] = true
		}
	}
	for _, index := range held.GetIndexes() {
		if nativeOnlyIndex(schema.IndexSpecification(index.GetDescription())) {
			names[index.GetDescription().GetName()] = true
		}
	}
	return names
}

// withoutNativeIndexes leaves out of an index input every name either side
// holds as a vector or full-text index: planNativeIndexes plans those.
func withoutNativeIndexes(in indexInput) indexInput {
	names := nativeIndexNames(in.desired, in.held)
	if len(names) == 0 {
		return in
	}
	desired, held := &native.IndexSetIntent{}, &native.IndexObservations{}
	for _, declaration := range in.desired.GetIndexes() {
		if !names[declaration.GetSpecification().GetName()] {
			desired.Indexes = append(desired.Indexes, declaration)
		}
	}
	for _, index := range in.held.GetIndexes() {
		if !names[index.GetDescription().GetName()] {
			held.Indexes = append(held.Indexes, index)
		}
	}
	in.desired, in.held = desired, held
	return in
}

// planNativeIndexes returns the changes that give an existing table the
// vector and full-text indexes the declaration states, in name order, or the
// reason they cannot be made. It plans every name either side holds as a
// vector or full-text index; planIndexes plans the others.
//
// YDB changes nothing of a vector or full-text index in place, so the rule is
// the host's embedded engine's: an index the declaration states and the table
// does not hold is added, one the table holds and the declaration leaves out
// is dropped, and one whose columns, kind or settings differ is dropped and
// added again under its name. A renamed index is one of each. An index the
// declaration states as one family under the name the table holds the other
// family under, a global index against a vector or full-text one, is refused:
// planIndexes refuses to rebuild a global index.
func planNativeIndexes(in nativeIndexInput) ([]indexChange, string) {
	if !in.managed {
		return nil, ""
	}
	names := nativeIndexNames(in.desired, in.held)
	wanted := make(map[string]*native.IndexIntent)
	for _, declaration := range in.desired.GetIndexes() {
		wanted[declaration.GetSpecification().GetName()] = declaration.GetSpecification()
	}
	held := make(map[string]*native.TableIndexDescription)
	for _, index := range in.held.GetIndexes() {
		held[index.GetDescription().GetName()] = index.GetDescription()
	}
	var changes []indexChange
	for _, name := range sortedNames(names) {
		want, declared := wanted[name]
		have, present := held[name]
		switch {
		case declared && present:
			holds := schema.IndexSpecification(have)
			if SameNativeIndex(want, holds) {
				if reason := keptColumns(want, in.columns); reason != "" {
					return nil, reason
				}
				continue
			}
			if !nativeOnlyIndex(want) || !nativeOnlyIndex(holds) {
				return nil, fmt.Sprintf("index %q: the table holds a %s index and the declaration states a %s index under its name; "+
					"this provider drops and adds an index under one name only when both are vector or full-text indexes", name, indexFamily(holds), indexFamily(want))
			}
			if reason := nativeIndexRefusal(want, in.columns, in.capabilities); reason != "" {
				return nil, reason
			}
			// A removal and an addition under one name: indexReplacements
			// orders the removal first.
			changes = append(changes, indexChange{drop: proto.CloneOf(have)}, indexChange{add: nativeIndexDefinition(want)})
		case declared:
			if reason := nativeIndexRefusal(want, in.columns, in.capabilities); reason != "" {
				return nil, reason
			}
			changes = append(changes, indexChange{add: nativeIndexDefinition(want)})
		case present:
			changes = append(changes, indexChange{drop: proto.CloneOf(have)})
		}
	}
	return changes, ""
}

func sortedNames(names map[string]bool) []string {
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

// indexFamily names the family of an index for a refusal: global, vector or
// full-text.
func indexFamily(spec *native.IndexIntent) string {
	switch spec.GetType().(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		return "vector"
	case *native.IndexIntent_GlobalFulltextPlainIndex, *native.IndexIntent_GlobalFulltextRelevanceIndex:
		return "full-text"
	}
	return "global"
}

// keptColumns requires an index the table keeps to read only columns the
// declaration states: the server refuses to drop a column an index reads, and
// would do so after dispatch.
func keptColumns(spec *native.IndexIntent, columns nativeIndexColumns) string {
	for _, column := range slices.Concat(spec.GetIndexColumns(), spec.GetDataColumns()) {
		if columns.types[column] == "" {
			return fmt.Sprintf("index %q reads column %q, which the declaration does not state", spec.GetName(), column)
		}
	}
	return ""
}

// declaredKeyTypes returns the YDB type of every column a declaration states,
// and its key in order, for the rules a vector or full-text index follows.
func declaredKeyTypes(table *protocol.DeclaredTable, intent *native.RowTableIntent) nativeIndexColumns {
	result := nativeIndexColumns{types: declaredTypes(table.GetColumns())}
	for _, column := range table.GetColumns() {
		if column.GetPrimary() && !slices.Contains(result.key, column.GetName()) {
			result.key = append(result.key, column.GetName())
		}
	}
	for _, name := range slices.Concat(table.GetPrimaryKey(), intent.GetPrimaryKey()) {
		if !slices.Contains(result.key, name) {
			result.key = append(result.key, name)
		}
	}
	return result
}

// creationIndexes are the vector and full-text indexes a new table declares
// in its native index set, each added by a step of its own after the table is
// created: the native creation call of the pinned bindings carries neither
// kind. A global index the set states never reaches it: the compilation of the
// creation refuses one (see tableFacets), since a new table takes its global
// indexes from the common declaration (see declaredIndexes).
func creationIndexes(owner *protocol.Reference, table *protocol.DeclaredTable, request *nativeCreation, capabilities map[string]bool) ([]indexChange, string, error) {
	facets, err := facetEnvelopes(owner, table.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return nil, "", err
	}
	if facets.indexes == nil {
		return nil, "", nil
	}
	var declared native.IndexSetIntent
	if err := protojson.Unmarshal(facets.indexes.Payload, &declared); err != nil {
		return nil, "", err
	}
	var changes []indexChange
	for _, declaration := range declared.GetIndexes() {
		spec := declaration.GetSpecification()
		if slices.Contains(request.names, spec.GetName()) {
			return nil, fmt.Sprintf("table %q: two of its indexes are named %q, and YDB names an index once per table", tableLabel(owner), spec.GetName()), nil
		}
		if reason := nativeIndexRefusal(spec, request.columns, capabilities); reason != "" {
			return nil, reason, nil
		}
		request.names = append(request.names, spec.GetName())
		changes = append(changes, indexChange{add: nativeIndexDefinition(spec)})
	}
	return changes, "", nil
}

// nativeCreation is what the vector and full-text index rule reads of a new
// table: the index names the creation request holds and the columns it
// creates with their YDB types and the key.
type nativeCreation struct {
	names   []string
	columns nativeIndexColumns
}

// plannedCreationIndexes returns the steps that add the vector and full-text
// indexes a new row table declares, after the creation captured in operation,
// or the reason they cannot be added on this target. A column table takes
// none: its compilation refuses the native index set.
func plannedCreationIndexes(owner *protocol.Reference, table *protocol.DeclaredTable, operation *protocol.NormalizedValue, capabilities map[string]bool) ([]*indexStep, string, error) {
	if isColumnCreation(operation) {
		return nil, "", nil
	}
	created, err := Request(operation.Value)
	if err != nil {
		return nil, "", err
	}
	creation := &nativeCreation{columns: nativeIndexColumns{types: make(map[string]string), key: slices.Clone(created.GetPrimaryKey())}}
	for _, index := range created.GetIndexes() {
		creation.names = append(creation.names, index.GetName())
	}
	for _, column := range created.GetColumns() {
		name, _, err := schema.ColumnType(column.GetType())
		if err != nil {
			return nil, "", err
		}
		creation.columns.types[column.GetName()] = name
	}
	changes, reason, err := creationIndexes(owner, table, creation, capabilities)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	var steps []*indexStep
	for _, change := range changes {
		step := &indexStep{change: change}
		if step.operation, err = CaptureNativeIndex("pending", owner, change.add); err != nil {
			return nil, err.Error(), nil
		}
		steps = append(steps, step)
	}
	return steps, "", nil
}

// creationStep is the planning step that adds the index to a table the step
// created creates, ordered after it.
func (index *indexStep) creationStep(number int, subject *protocol.ComparisonIdentity, created *protocol.StepIdentity) (*protocol.PlanningStep, *protocol.PlanningDependency) {
	index.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-create-index/%06d", number)}
	index.operation.Id = index.id.Name
	facts := index.operation.OperationSemantics
	step := &protocol.PlanningStep{Id: index.id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: index.operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
				{Subject: indexSubject(subject, index.change.name()), Action: protocol.PlanningEffect_CREATE}}}}
	return step, &protocol.PlanningDependency{Before: proto.CloneOf(created), After: proto.CloneOf(index.id)}
}

// rebuiltNativeIndex names a vector or full-text index the table of a rebuild
// declares or holds, or returns the empty string. The new table is created
// with the global indexes of its declaration alone.
func rebuiltNativeIndex(table *protocol.PlanningTable) string {
	for _, record := range table.GetDesired().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() != schema.IndexProperties {
			continue
		}
		var declared native.IndexSetIntent
		if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &declared); err == nil {
			for _, declaration := range declared.GetIndexes() {
				if nativeOnlyIndex(declaration.GetSpecification()) {
					return declaration.GetSpecification().GetName()
				}
			}
		}
	}
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() != schema.IndexProperties {
			continue
		}
		var held native.IndexObservations
		if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err == nil {
			for _, index := range held.GetIndexes() {
				if nativeOnlyIndex(schema.IndexSpecification(index.GetDescription())) {
					return index.GetDescription().GetName()
				}
			}
		}
	}
	return ""
}
