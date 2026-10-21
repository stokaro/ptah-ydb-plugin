package schema

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// IndexProperties identifies the complete observed index set of one table.
const IndexProperties = modelNamespace + "table-indexes"

// ValidateIndexes refuses incomplete, unknown, or duplicate native indexes before
// their names are used to read physical tables.
func ValidateIndexes(values []*native.TableIndexDescription) error {
	seen := map[string]bool{}
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
		seen := map[string]bool{}
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
	return capture(IndexProperties, owner, value)
}

func normalizeIndexes(value *native.IndexObservations) error {
	seen := map[string]bool{}
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
		names, err := ImplementationNames(index.Description)
		if err != nil {
			return err
		}
		if len(names) != len(index.Tables) {
			return fmt.Errorf("incomplete YDB index implementation tables")
		}
		tables := map[string]bool{}
		for _, table := range index.Tables {
			if !slices.Contains(names, table.GetName()) || tables[table.GetName()] {
				return fmt.Errorf("unexpected or duplicate YDB index implementation table")
			}
			tables[table.GetName()] = true
			if err := validateImplementation(table.GetDescription()); err != nil {
				return err
			}
		}
		slices.SortFunc(index.Tables, func(a, b *native.ImplementationTable) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(value.Indexes, func(a, b *native.ObservedIndex) int { return strings.Compare(a.Description.Name, b.Description.Name) })
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
	if table.GetTableStats() != nil || table.GetSelf().GetName() != "" || table.GetSelf().GetType() != 0 || table.GetSelf().GetSizeBytes() != 0 || table.GetSelf().GetCreatedAt() != nil {
		return fmt.Errorf("index implementation properties contain transient metadata")
	}
	if table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED && table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_ROW {
		return fmt.Errorf("index implementation requires row storage")
	}
	table.StoreType = Ydb_Table.StoreType_STORE_TYPE_ROW
	columns := map[string]bool{}
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
	keys := map[string]bool{}
	for _, name := range table.GetPrimaryKey() {
		if !columns[name] || keys[name] {
			return fmt.Errorf("invalid YDB index implementation primary key")
		}
		keys[name] = true
	}
	return nil
}
