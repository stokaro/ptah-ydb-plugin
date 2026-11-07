package schema

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// IndexProperties identifies the complete observed index set of one table.
const IndexProperties = modelNamespace + "table-indexes"

// ValidateIndexes refuses incomplete, unknown, or duplicate native indexes before
// their names are used to read physical tables.
func ValidateIndexes(values []*native.TableIndexDescription) error {
	seen := make(map[string]bool)
	for _, value := range values {
		if err := validateIndex(value); err != nil {
			return err
		}
		if seen[value.Name] {
			return fmt.Errorf("duplicate YDB index name")
		}
		seen[value.Name] = true
	}
	return nil
}

// CommonIndex reports an index the common model describes: a global secondary
// index, synchronous, asynchronous or unique, which a host that negotiates
// schema.observed-indexes compares and changes by its key and covered columns.
// A vector or full-text index has settings the common model has no place for.
func CommonIndex(index *native.TableIndexDescription) bool {
	switch index.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex, *native.TableIndexDescription_GlobalAsyncIndex, *native.TableIndexDescription_GlobalUniqueIndex:
		return true
	}
	return false
}

// ImplementationNames names the physical tables YDB uses for this index kind.
// The mapping follows NTableIndex::GetImplTables at the pinned upstream commit
// recorded in third_party/ydb/NOTICE.md. A vector prefix is a separate table.
func ImplementationNames(index *native.TableIndexDescription) ([]string, error) {
	switch index.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex, *native.TableIndexDescription_GlobalAsyncIndex, *native.TableIndexDescription_GlobalUniqueIndex, *native.TableIndexDescription_GlobalFulltextPlainIndex:
		return []string{"indexImplTable"}, nil
	case *native.TableIndexDescription_GlobalVectorKmeansTreeIndex:
		names := []string{"indexImplLevelTable", "indexImplPostingTable"}
		if len(index.GetIndexColumns()) > 1 {
			names = append(names, "indexImplPrefixTable")
		}
		return names, nil
	case *native.TableIndexDescription_GlobalFulltextRelevanceIndex:
		return []string{"indexImplDictTable", "indexImplDocsTable", "indexImplStatsTable", "indexImplTable"}, nil
	default:
		return nil, fmt.Errorf("unsupported YDB index kind")
	}
}

func validateIndex(index *native.TableIndexDescription) error {
	if err := unreadIndexKind(index); err != nil {
		return err
	}
	if err := Known(index); err != nil {
		return err
	}
	if !componentName(index.GetName()) || len(index.GetIndexColumns()) == 0 {
		return fmt.Errorf("invalid YDB index identity or columns")
	}
	if index.GetStatus() != native.TableIndexDescription_STATUS_READY {
		return fmt.Errorf("YDB index is not ready for schema comparison")
	}
	for _, columns := range [][]string{index.GetIndexColumns(), index.GetDataColumns()} {
		seen := make(map[string]bool)
		for _, column := range columns {
			if column == "" || seen[column] {
				return fmt.Errorf("invalid or duplicate YDB index column")
			}
			seen[column] = true
		}
	}
	_, err := ImplementationNames(index)
	return err
}

// unreadIndexKinds names, by its field number in YDB's index description, each
// kind of index the description carries in a field this provider's model does
// not hold, as the host's reader named it.
var unreadIndexKinds = map[protowire.Number]string{
	12: "bloom_filter index",
	13: "bloom_ngram_filter index",
	14: "global JSON index",
	15: "min_max index",
}

// unreadIndexKind refuses an index whose description carries a field the
// provider's model does not hold, naming the index and its kind, where the
// field is one of an index kind: the refusal says which index the provider
// does not read rather than which message it could not decode.
func unreadIndexKind(index *native.TableIndexDescription) error {
	unknown := index.ProtoReflect().GetUnknown()
	if len(unknown) == 0 {
		return nil
	}
	number, _, length := protowire.ConsumeTag(unknown)
	if length < 0 {
		return nil
	}
	kind, known := unreadIndexKinds[number]
	if !known {
		kind = "index of a kind this provider does not know"
	}
	return fmt.Errorf("index %q is a %s, which this provider does not read", index.GetName(), kind)
}

func componentName(name string) bool {
	return utf8.ValidString(name) && name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// CaptureIndexes binds native index kinds/settings to the implementation-table
// descriptions actually read from YDB. No prefix is returned for missing data.
func CaptureIndexes(owner *protocol.Reference, observed *native.IndexObservations) (*protocol.Envelope, error) {
	if err := Known(observed); err != nil {
		return nil, err
	}
	value := proto.CloneOf(observed)
	for _, index := range value.Indexes {
		if index == nil || index.Description == nil {
			return nil, fmt.Errorf("missing YDB index observation")
		}
		index.Description.SizeBytes = 0
		for _, table := range index.Tables {
			if table == nil || table.Description == nil {
				return nil, fmt.Errorf("missing YDB index implementation observation")
			}
			for _, column := range table.Description.Columns {
				if sequence := column.GetFromSequence(); sequence != nil {
					sequence.SetVal = nil
				}
			}
			table.Description.TableStats = nil
			if self := table.Description.Self; self != nil {
				self.Name = ""
				self.Type = 0
				self.SizeBytes = 0
				self.CreatedAt = nil
			}
		}
	}
	for _, index := range value.CommonIndexes {
		if index == nil {
			return nil, fmt.Errorf("missing YDB index observation")
		}
		index.SizeBytes = 0
	}
	return capture(IndexProperties, owner, value)
}

// validateImplementationTables checks the tables that implement an index:
// each one YDB uses for its kind, once, or none for a vector or full-text
// index (see normalizeIndexes).
func validateImplementationTables(index *native.ObservedIndex) error {
	names, err := ImplementationNames(index.Description)
	if err != nil {
		return err
	}
	if len(names) != len(index.Tables) && (len(index.Tables) != 0 || CommonIndex(index.Description)) {
		return fmt.Errorf("incomplete YDB index implementation tables")
	}
	tables := make(map[string]bool)
	for _, table := range index.Tables {
		if !slices.Contains(names, table.GetName()) || tables[table.GetName()] {
			return fmt.Errorf("unexpected or duplicate YDB index implementation table")
		}
		tables[table.GetName()] = true
		if err := validateImplementation(table.GetDescription()); err != nil {
			return err
		}
	}
	return nil
}

// normalizeIndexes validates and sorts an index set. A read describes every
// table that implements each index. A declaration, and the observation the
// host predicts from one, may state none of them for a vector or full-text
// index: a source declares one without the tables YDB gives it, and the
// planner writes none.
func normalizeIndexes(value *native.IndexObservations) error {
	seen := make(map[string]bool)
	for _, index := range value.GetIndexes() {
		if err := validateIndex(index.GetDescription()); err != nil {
			return err
		}
		if seen[index.Description.Name] {
			return fmt.Errorf("duplicate observed YDB index")
		}
		seen[index.Description.Name] = true
		if index.Description.SizeBytes != 0 {
			return fmt.Errorf("index properties cannot contain runtime statistics")
		}
		if err := validateImplementationTables(index); err != nil {
			return err
		}
		slices.SortFunc(index.Tables, func(a, b *native.ImplementationTable) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(value.Indexes, func(a, b *native.ObservedIndex) int { return strings.Compare(a.Description.Name, b.Description.Name) })
	// A common index is described for its settings alone. Its status is not
	// read: the common model compares the index, and a rebuild reads only
	// its settings.
	for _, index := range value.GetCommonIndexes() {
		if err := Known(index); err != nil {
			return err
		}
		if !componentName(index.GetName()) || len(index.GetIndexColumns()) == 0 || !CommonIndex(index) {
			return fmt.Errorf("invalid YDB common index description")
		}
		if seen[index.GetName()] {
			return fmt.Errorf("duplicate observed YDB index")
		}
		seen[index.GetName()] = true
		if index.GetSizeBytes() != 0 {
			return fmt.Errorf("index properties cannot contain runtime statistics")
		}
	}
	slices.SortFunc(value.CommonIndexes, func(a, b *native.TableIndexDescription) int { return strings.Compare(a.Name, b.Name) })
	return nil
}

func validateImplementation(table *native.DescribeTableResult) error {
	if err := Known(table); err != nil {
		return err
	}
	if len(table.GetColumns()) == 0 || len(table.GetPrimaryKey()) == 0 {
		return fmt.Errorf("incomplete YDB index implementation schema")
	}
	if len(table.GetIndexes()) != 0 || len(table.GetChangefeeds()) != 0 {
		return fmt.Errorf("unexpected nested YDB index implementation objects")
	}
	if table.GetTableStats() != nil || transientSelf(table.GetSelf()) {
		return fmt.Errorf("index implementation properties contain transient metadata")
	}
	if table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED && table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_ROW {
		return fmt.Errorf("index implementation requires row storage")
	}
	table.StoreType = Ydb_Table.StoreType_STORE_TYPE_ROW
	columns := make(map[string]bool)
	for _, column := range table.GetColumns() {
		if column.GetName() == "" || columns[column.GetName()] {
			return fmt.Errorf("invalid YDB index implementation column")
		}
		if column.GetFromSequence().GetSetVal() != nil {
			return fmt.Errorf("index implementation contains a runtime sequence position")
		}
		columns[column.GetName()] = true
		if _, _, err := ColumnType(column.GetType()); err != nil {
			return err
		}
	}
	keys := make(map[string]bool)
	for _, name := range table.GetPrimaryKey() {
		if !columns[name] || keys[name] {
			return fmt.Errorf("invalid YDB index implementation primary key")
		}
		keys[name] = true
	}
	return nil
}
