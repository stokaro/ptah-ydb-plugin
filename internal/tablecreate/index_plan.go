package tablecreate

import (
	"fmt"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// indexChange is one index an alteration adds to a table or drops from it. An
// addition carries the declared specification, a removal the index as the table
// held it. YDB cannot alter an index in place, so a change never replaces one.
type indexChange struct {
	add  *native.IndexIntent
	drop *native.TableIndexDescription
}

func (c indexChange) name() string {
	if c.add != nil {
		return c.add.Name
	}
	return c.drop.Name
}

// columns returns the columns the index reads: its keys and the columns it covers.
func (c indexChange) columns() []string {
	if c.add != nil {
		return slices.Concat(c.add.IndexColumns, c.add.DataColumns)
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

// indexSignature is everything about an index a comparison reads.
type indexSignature struct {
	kind    string
	keys    []string
	covered []string
	// detail is the encoded kind-specific message, empty when the kind states
	// no settings. A held global index carries none, whatever the server reports.
	detail string
}

func (s indexSignature) equal(other indexSignature) bool {
	return s.kind == other.kind && slices.Equal(s.keys, other.keys) && slices.Equal(s.covered, other.covered) && s.detail == other.detail
}

// scope names what puts an index outside the global secondary indexes this
// service adds and drops, or returns the empty string.
func (s indexSignature) scope() string {
	switch {
	case s.kind != "global" && s.kind != "global async" && s.kind != "global unique":
		return "is a " + s.kind + " index"
	case s.detail != "":
		return "states settings of its own"
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

// signatureOf reads one specification. held is true for the specification of an
// index the table holds. The server reports the partitioning of the table that
// implements a global index, defaults included, so the settings of a held global
// index say nothing about what was declared and are not compared; the settings a
// declaration states are, and are refused.
func signatureOf(spec *native.IndexIntent, held bool) indexSignature {
	result := indexSignature{keys: spec.IndexColumns, covered: spec.DataColumns}
	stated := func(settings *native.GlobalIndexSettings) string {
		if held {
			return ""
		}
		return detail(settings)
	}
	switch kind := spec.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		result.kind, result.detail = "global", stated(kind.GlobalIndex.GetSettings())
	case *native.IndexIntent_GlobalAsyncIndex:
		result.kind, result.detail = "global async", stated(kind.GlobalAsyncIndex.GetSettings())
	case *native.IndexIntent_GlobalUniqueIndex:
		result.kind, result.detail = "global unique", stated(kind.GlobalUniqueIndex.GetSettings())
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
func indexSignatures(specs []*native.IndexIntent, held bool) (names []string, signatures map[string]indexSignature, specByName map[string]*native.IndexIntent) {
	signatures, specByName = map[string]indexSignature{}, map[string]*native.IndexIntent{}
	for _, spec := range specs {
		names = append(names, spec.Name)
		signatures[spec.Name] = signatureOf(spec, held)
		specByName[spec.Name] = spec
	}
	slices.Sort(names)
	return names, signatures, specByName
}

func declaredSpecs(set *native.IndexSetIntent) []*native.IndexIntent {
	var result []*native.IndexIntent
	for _, declaration := range set.GetIndexes() {
		result = append(result, declaration.GetSpecification())
	}
	return result
}

func heldSpecs(observed *native.IndexObservations) (specs []*native.IndexIntent, held map[string]*native.TableIndexDescription) {
	held = map[string]*native.TableIndexDescription{}
	for _, index := range observed.GetIndexes() {
		specs = append(specs, schema.IndexSpecification(index.GetDescription()))
		held[index.GetDescription().GetName()] = index.GetDescription()
	}
	return specs, held
}

// planIndexes returns the additions and removals that give the table the index
// set the declaration states, in name order, or the reason it cannot be made.
// An empty result means the table already satisfies the declaration.
//
// Only global secondary indexes are added and dropped: sync, async and unique.
// A vector or fulltext index, and an index that states settings of its own,
// are refused by name rather than ignored. An index the table holds under a
// declared name but with another definition is refused too, because YDB cannot
// alter an index in place and a drop followed by an addition rebuilds it.
func planIndexes(in indexInput) ([]indexChange, string) {
	if !in.managed {
		return nil, ""
	}
	wantedSpecs := declaredSpecs(in.desired)
	heldSpecList, heldByName := heldSpecs(in.held)
	wantedNames, wanted, wantedByName := indexSignatures(wantedSpecs, false)
	heldNames, held, _ := indexSignatures(heldSpecList, true)
	var changes []indexChange
	for _, name := range slices.Compact(slices.Sorted(slices.Values(slices.Concat(wantedNames, heldNames)))) {
		want, declared := wanted[name]
		have, present := held[name]
		switch {
		case declared && present:
			if want.equal(have) {
				continue
			}
			for _, side := range []indexSignature{want, have} {
				if scope := side.scope(); scope != "" {
					return nil, fmt.Sprintf("index %q is outside the global secondary indexes this provider alters: it %s", name, scope)
				}
			}
			return nil, fmt.Sprintf("index %q holds a %s, but the declaration states a %s; YDB cannot alter an index in place, and dropping and adding it rebuilds the index", name, have.describe(), want.describe())
		case declared:
			if scope := want.scope(); scope != "" {
				return nil, fmt.Sprintf("the declared index %q is outside the global secondary indexes this provider alters: it %s", name, scope)
			}
			if reason := declaredColumns(name, want, in.columns); reason != "" {
				return nil, reason
			}
			changes = append(changes, indexChange{add: proto.CloneOf(wantedByName[name])})
		case present:
			if scope := have.scope(); scope != "" {
				return nil, fmt.Sprintf("the index %q the declaration leaves out is outside the global secondary indexes this provider alters: it %s", name, scope)
			}
			changes = append(changes, indexChange{drop: proto.CloneOf(heldByName[name])})
		}
	}
	// A kept index that reads a column the declaration no longer states would
	// make the server refuse the column removal after dispatch.
	for _, name := range heldNames {
		if _, declared := wanted[name]; declared {
			if reason := declaredColumns(name, held[name], in.columns); reason != "" {
				return nil, reason
			}
		}
	}
	slices.SortFunc(changes, func(a, b indexChange) int { return strings.Compare(a.name(), b.name()) })
	return changes, ""
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
	wantedNames, wanted, _ := indexSignatures(declaredSpecs(want), false)
	heldList, _ := heldSpecs(held)
	heldNames, have, _ := indexSignatures(heldList, true)
	if !slices.Equal(wantedNames, heldNames) {
		return true
	}
	return slices.ContainsFunc(wantedNames, func(name string) bool { return !wanted[name].equal(have[name]) })
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
