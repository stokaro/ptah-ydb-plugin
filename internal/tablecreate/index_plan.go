package tablecreate

import (
	"fmt"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// indexChange is one index an alteration adds to a table or drops from it. An
// addition carries the declared specification, a removal the index as the table
// held it. YDB cannot alter an index in place, so a change never replaces one.
type indexChange struct {
	add  *native.IndexIntent
	drop *native.TableIndexDescription
	// rename is an index the host renames in place, nil for any other change.
	rename *indexRename
	// source is the host's step the change replaces, or nil for a change of
	// the native index set.
	source *protocol.CommonPlanningStep
}

// indexRename is an index renamed in place: held is the index as the table
// holds it, under its name, and to is the name the declaration gives it.
type indexRename struct {
	held *native.TableIndexDescription
	to   string
}

// name is the name the change leaves the index with, or the name of the index
// it drops.
func (c indexChange) name() string {
	switch {
	case c.add != nil:
		return c.add.Name
	case c.rename != nil:
		return c.rename.to
	}
	return c.drop.Name
}

// columns returns the columns the index reads: its keys and the columns it covers.
func (c indexChange) columns() []string {
	switch {
	case c.add != nil:
		return slices.Concat(c.add.IndexColumns, c.add.DataColumns)
	case c.rename != nil:
		return slices.Concat(c.rename.held.IndexColumns, c.rename.held.DataColumns)
	}
	return slices.Concat(c.drop.IndexColumns, c.drop.DataColumns)
}

// indexInput is what the index rule reads. managed is true when the declaration
// states the index set of the table: an index the table holds and the
// declaration leaves out is then dropped. A declaration with no index set
// leaves the indexes alone. columns are the columns the declaration states.
type indexInput struct {
	managed bool
	desired *native.IndexSetIntent
	held    *native.IndexObservations
	columns []string
}

// indexSignature is everything about an index's definition a comparison
// reads. The partitioning and read replicas of a global index are not part of
// its definition: YDB changes them in place.
type indexSignature struct {
	kind    string
	keys    []string
	covered []string
	// detail is the encoded kind-specific message of a vector or fulltext
	// index, empty when the kind states no settings. A global sync, async or
	// unique index carries its settings in settings instead.
	detail string
	// settings are the partitioning and read replicas of a global sync, async
	// or unique index, nil for another kind.
	settings *native.GlobalIndexSettings
}

func (s indexSignature) equal(other indexSignature) bool {
	return s.kind == other.kind && slices.Equal(s.keys, other.keys) && slices.Equal(s.covered, other.covered) && s.detail == other.detail
}

// scope names what puts an index outside the global secondary indexes this
// service adds and drops, or returns the empty string.
func (s indexSignature) scope() string {
	if s.kind != "global" && s.kind != "global async" && s.kind != "global unique" {
		return "is a " + s.kind + " index"
	}
	return ""
}

func (s indexSignature) describe() string {
	text := fmt.Sprintf("%s index on (%s)", s.kind, strings.Join(s.keys, ", "))
	if len(s.covered) > 0 {
		text += fmt.Sprintf(" covering (%s)", strings.Join(s.covered, ", "))
	}
	return text
}

func detail(message proto.Message) string {
	if message == nil || proto.Size(message) == 0 {
		return ""
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return err.Error()
	}
	return string(encoded)
}

// signatureOf reads one specification. The server describes a global index
// with its partitioning and read replicas, defaults included, so the settings
// of a held global index are what it holds rather than what was declared. They
// are not part of its definition: the settings rule reads them as a table's
// (see planIndexSettings).
func signatureOf(spec *native.IndexIntent) indexSignature {
	result := indexSignature{keys: spec.IndexColumns, covered: spec.DataColumns}
	switch kind := spec.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		result.kind, result.settings = "global", kind.GlobalIndex.GetSettings()
	case *native.IndexIntent_GlobalAsyncIndex:
		result.kind, result.settings = "global async", kind.GlobalAsyncIndex.GetSettings()
	case *native.IndexIntent_GlobalUniqueIndex:
		result.kind, result.settings = "global unique", kind.GlobalUniqueIndex.GetSettings()
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		result.kind, result.detail = "vector", detail(kind.GlobalVectorKmeansTreeIndex)
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		result.kind, result.detail = "fulltext plain", detail(kind.GlobalFulltextPlainIndex)
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		result.kind, result.detail = "fulltext relevance", detail(kind.GlobalFulltextRelevanceIndex)
	default:
		result.kind = "unknown"
	}
	return result
}

// indexSignatures reads the indexes of one side by name, in name order.
func indexSignatures(specs []*native.IndexIntent) (names []string, signatures map[string]indexSignature, specByName map[string]*native.IndexIntent) {
	signatures, specByName = map[string]indexSignature{}, map[string]*native.IndexIntent{}
	for _, spec := range specs {
		names = append(names, spec.Name)
		signatures[spec.Name] = signatureOf(spec)
		specByName[spec.Name] = spec
	}
	slices.Sort(names)
	return names, signatures, specByName
}

func declaredSpecs(set *native.IndexSetIntent) []*native.IndexIntent {
	var result []*native.IndexIntent
	for _, declaration := range set.GetIndexes() {
		result = append(result, comparableIndex(declaration.GetSpecification()))
	}
	return result
}

func heldSpecs(observed *native.IndexObservations) (specs []*native.IndexIntent, held map[string]*native.TableIndexDescription) {
	held = map[string]*native.TableIndexDescription{}
	for _, index := range observed.GetIndexes() {
		specs = append(specs, comparableIndex(schema.IndexSpecification(index.GetDescription())))
		held[index.GetDescription().GetName()] = index.GetDescription()
	}
	return specs, held
}

// planIndexes returns the additions and removals that give the table the index
// set the declaration states, in name order, with the changes of the
// partitioning and read replicas of the indexes it keeps, or the reason it
// cannot be made. An empty result means the table already satisfies the
// declaration.
//
// Only global secondary indexes are added and dropped here: sync, async and
// unique. An index name either side holds as a vector or full-text index is
// left to planNativeIndexes. An index to add that states its partitioning or
// read replicas is refused by name rather than ignored. An index the table
// holds under a declared name but with another definition is refused too,
// because YDB cannot alter an index in place and a drop followed by an
// addition rebuilds it. Its partitioning and read replicas YDB changes in
// place (see planIndexSettings).
func planIndexes(in indexInput) ([]indexChange, []indexSettingsChange, string) {
	if !in.managed {
		return nil, nil, ""
	}
	// planNativeIndexes plans the vector and full-text indexes.
	in = withoutNativeIndexes(in)
	wantedSpecs := declaredSpecs(in.desired)
	heldSpecList, heldByName := heldSpecs(in.held)
	wantedNames, wanted, wantedByName := indexSignatures(wantedSpecs)
	heldNames, held, _ := indexSignatures(heldSpecList)
	var changes []indexChange
	var settings []indexSettingsChange
	for _, name := range slices.Compact(slices.Sorted(slices.Values(slices.Concat(wantedNames, heldNames)))) {
		want, declared := wanted[name]
		have, present := held[name]
		switch {
		case declared && present:
			if !want.equal(have) {
				for _, side := range []indexSignature{want, have} {
					if scope := side.scope(); scope != "" {
						return nil, nil, fmt.Sprintf("index %q is outside the global secondary indexes this provider alters: it %s", name, scope)
					}
				}
				return nil, nil, fmt.Sprintf("index %q holds a %s, but the declaration states a %s; YDB cannot alter an index in place, and dropping and adding it rebuilds the index", name, have.describe(), want.describe())
			}
			change, reason := planIndexSettings(name, want.settings, heldByName[name])
			if reason != "" {
				return nil, nil, reason
			}
			if change != nil {
				settings = append(settings, *change)
			}
		case declared:
			if scope := want.scope(); scope != "" {
				return nil, nil, fmt.Sprintf("the declared index %q is outside the global secondary indexes this provider alters: it %s", name, scope)
			}
			if reason := declaredColumns(name, want, in.columns); reason != "" {
				return nil, nil, reason
			}
			added := withoutIndexSettings(wantedByName[name])
			changes = append(changes, indexChange{add: added})
			// No call that adds an index takes its settings, so the index
			// takes YDB's and a change of its own gives it the declared ones,
			// as the host's embedded engine writes ALTER INDEX ... SET after
			// ADD INDEX.
			change, reason := planIndexSettings(name, want.settings, newIndexDescription(added))
			if reason != "" {
				return nil, nil, reason
			}
			if change != nil {
				settings = append(settings, *change)
			}
		case present:
			if scope := have.scope(); scope != "" {
				return nil, nil, fmt.Sprintf("the index %q the declaration leaves out is outside the global secondary indexes this provider alters: it %s", name, scope)
			}
			changes = append(changes, indexChange{drop: proto.CloneOf(heldByName[name])})
		}
	}
	// A kept index that reads a column the declaration no longer states would
	// make the server refuse the column removal after dispatch.
	for _, name := range heldNames {
		if _, declared := wanted[name]; declared {
			if reason := declaredColumns(name, held[name], in.columns); reason != "" {
				return nil, nil, reason
			}
		}
	}
	slices.SortFunc(changes, func(a, b indexChange) int { return strings.Compare(a.name(), b.name()) })
	return changes, settings, ""
}

// declaredColumns requires every column an index reads to be one the
// declaration states.
func declaredColumns(name string, signature indexSignature, columns []string) string {
	for _, column := range slices.Concat(signature.keys, signature.covered) {
		if !slices.Contains(columns, column) {
			return fmt.Sprintf("index %q reads column %q, which the declaration does not state", name, column)
		}
	}
	return ""
}

// indexDiffers reports whether the table holding the observed indexes does not
// already satisfy the declared index set. A difference the planner refuses
// counts: the comparison reports it so that the refusal reaches the user.
func indexDiffers(want *native.IndexSetIntent, held *native.IndexObservations) bool {
	wantedNames, wanted, _ := indexSignatures(declaredSpecs(want))
	heldList, heldByName := heldSpecs(held)
	heldNames, have, _ := indexSignatures(heldList)
	if !slices.Equal(wantedNames, heldNames) {
		return true
	}
	return slices.ContainsFunc(wantedNames, func(name string) bool {
		if !wanted[name].equal(have[name]) {
			return true
		}
		change, reason := planIndexSettings(name, wanted[name].settings, heldByName[name])
		return change != nil || reason != ""
	})
}

// tableIndex converts a declared specification to the native request form.
func tableIndex(spec *native.IndexIntent) (*Ydb_Table.TableIndex, error) {
	result := &Ydb_Table.TableIndex{Name: spec.Name, IndexColumns: slices.Clone(spec.IndexColumns), DataColumns: slices.Clone(spec.DataColumns)}
	switch spec.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		result.Type = &Ydb_Table.TableIndex_GlobalIndex{GlobalIndex: &Ydb_Table.GlobalIndex{}}
	case *native.IndexIntent_GlobalAsyncIndex:
		result.Type = &Ydb_Table.TableIndex_GlobalAsyncIndex{GlobalAsyncIndex: &Ydb_Table.GlobalAsyncIndex{}}
	case *native.IndexIntent_GlobalUniqueIndex:
		result.Type = &Ydb_Table.TableIndex_GlobalUniqueIndex{GlobalUniqueIndex: &Ydb_Table.GlobalUniqueIndex{}}
	default:
		return nil, fmt.Errorf("YDB adds only global sync, async and unique indexes to an existing table")
	}
	return result, nil
}

// withoutIndexSettings is a copy of a global sync, async or unique index
// without the settings of its kind: the call that adds it takes none.
func withoutIndexSettings(spec *native.IndexIntent) *native.IndexIntent {
	result := proto.CloneOf(spec)
	switch kind := result.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		kind.GlobalIndex.Settings = nil
	case *native.IndexIntent_GlobalAsyncIndex:
		kind.GlobalAsyncIndex.Settings = nil
	case *native.IndexIntent_GlobalUniqueIndex:
		kind.GlobalUniqueIndex.Settings = nil
	}
	return result
}

// newIndexDescription is the index added as the table describes it once YDB
// builds it: the settings a new index takes whatever its table holds, measured
// on 25.1.4.7 and 26.2.1.14 as automatic partitioning by size at 2048 MB, none
// by load, at least one partition, and no read replicas.
func newIndexDescription(added *native.IndexIntent) *native.TableIndexDescription {
	settings := &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{
		PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}}
	result := &native.TableIndexDescription{Name: added.Name, IndexColumns: slices.Clone(added.IndexColumns), DataColumns: slices.Clone(added.DataColumns)}
	switch added.Type.(type) {
	case *native.IndexIntent_GlobalAsyncIndex:
		result.Type = &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{Settings: settings}}
	case *native.IndexIntent_GlobalUniqueIndex:
		result.Type = &native.TableIndexDescription_GlobalUniqueIndex{GlobalUniqueIndex: &native.GlobalUniqueIndex{Settings: settings}}
	default:
		result.Type = &native.TableIndexDescription_GlobalIndex{GlobalIndex: &native.GlobalIndex{Settings: settings}}
	}
	return result
}
