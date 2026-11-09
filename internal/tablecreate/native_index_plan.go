package tablecreate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
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
// family under, a global index against a vector or full-text one, is planned
// together with the host's common index step by alterationSides.indexes.
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
// the dimension each VECTOR(n) column names, and its key in order, for the
// rules a vector or full-text index follows. A column reads as the type the
// session's creation of it writes, as the host's embedded engine read it:
// VECTOR(n) as the String column YDB keeps a vector in.
func declaredKeyTypes(table *protocol.DeclaredTable, intent *native.RowTableIntent, caps map[string]bool) nativeIndexColumns {
	created := make([]*protocol.DeclaredColumn, 0, len(table.GetColumns()))
	for _, column := range table.GetColumns() {
		spelling := column.GetType()
		if mapping, err := ydbtype.Map(spelling, ydbtype.Capabilities(caps)); err == nil {
			spelling = mapping.Type
		}
		created = append(created, &protocol.DeclaredColumn{Name: column.GetName(), Type: spelling})
	}
	result := nativeIndexColumns{types: declaredTypes(created), dimensions: vectorDimensions(table.GetColumns(), caps)}
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

// vectorDimensions returns the dimension each column declared VECTOR(n)
// names. The column keeps none (see ydbtype.Map), and a vector index over it
// is held to it.
func vectorDimensions(columns []*protocol.DeclaredColumn, caps map[string]bool) map[string]uint64 {
	result := make(map[string]uint64)
	for _, column := range columns {
		if mapping, err := ydbtype.Map(column.GetType(), ydbtype.Capabilities(caps)); err == nil && mapping.Dimension != 0 {
			result[column.GetName()] = mapping.Dimension
		}
	}
	return result
}

// creationIndexes are the vector and full-text indexes a new table declares
// in its native index set, each built with the table by its native creation
// (see withCreationIndexes). A global index the set states never reaches it:
// the compilation of the creation refuses one (see tableFacets), since a new
// table takes its global indexes from the common declaration (see
// declaredIndexes).
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

// withCreationIndexes returns the creation captured in operation with the
// vector and full-text indexes a new row table declares built into it, or the
// reason they cannot be built on this target. The host's embedded engine at
// 74de0b65b wrote every index of a new table inside its CREATE TABLE, in name
// order, and created them with the table, on 25.1.4.7 and 26.2.1.14; the
// request holds the indexes in that order, and its text form writes them so.
// A column table takes none: its compilation refuses the native index set.
func withCreationIndexes(owner *protocol.Reference, table *protocol.DeclaredTable, operation *protocol.NormalizedValue, capabilities map[string]bool) (*protocol.NormalizedValue, string, error) {
	if isColumnCreation(operation) {
		return operation, "", nil
	}
	created, err := Request(operation.Value)
	if err != nil {
		return nil, "", err
	}
	creation := &nativeCreation{columns: nativeIndexColumns{types: make(map[string]string), dimensions: vectorDimensions(table.GetColumns(), capabilities),
		key: slices.Clone(created.GetPrimaryKey())}}
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
	if err != nil || reason != "" || len(changes) == 0 {
		return operation, reason, err
	}
	for _, change := range changes {
		index, err := schema.RequestIndex(change.add)
		if err != nil {
			return nil, "", err
		}
		created.Indexes = append(created.Indexes, index)
	}
	slices.SortFunc(created.Indexes, func(a, b *Ydb_Table.TableIndex) int { return strings.Compare(a.GetName(), b.GetName()) })
	result, err := CaptureRequest(operation.GetId(), owner, created)
	if err != nil {
		return nil, err.Error(), nil
	}
	return result, "", nil
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

// SourcesStateNativeIndexes reports whether the sources of a host with the
// target features features declare the vector and full-text indexes of a
// table: a host that negotiates source.index-kinds sends each such
// declaration to this provider. A source that describes the native index sets
// of every table it declares then declares a table that writes none to have
// none. Without the feature a source cannot declare one, and a table keeps
// the native indexes it holds.
func SourcesStateNativeIndexes(features []string) bool {
	return slices.Contains(features, provider.IndexKindsFeature)
}

// UnstatedIndexFacet returns the desired native index set of a table whose
// source states none: the empty set (see SourcesStateNativeIndexes).
func UnstatedIndexFacet(owner *protocol.Reference) (*protocol.Envelope, error) {
	payload, err := protojson.Marshal(&native.IndexSetIntent{})
	if err != nil {
		return nil, err
	}
	model := schema.NativeDesiredModel(schema.IndexProperties)
	return schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(owner), Payload: payload})
}

// nativeIndexesManaged reports whether the declaration of table states its
// vector and full-text indexes: it carries the native index set, or its
// source states the native index sets of every table it declares (see
// SourcesStateNativeIndexes) and the read holds the table's. The comparison
// reports such a table's native indexes through compareUnstatedIndexes, and
// this is the planner's side of the same rule.
func nativeIndexesManaged(table *protocol.PlanningTable, desired, observed capturedFacets, features []string) bool {
	return desired.indexes != nil || observed.indexes != nil && SourcesStateNativeIndexes(features) &&
		FacetKnown(table.GetDesired().GetFeatures().GetFacets().GetCoverage(), schema.IndexProperties, table.GetSubject(), DesiredSide)
}
