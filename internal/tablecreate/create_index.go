package tablecreate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// declaredIndexes compiles the indexes a table declares into the indexes its
// native creation builds with the table, as the host's embedded engine writes
// them inside its CREATE TABLE. That is the only way to give a new table a
// unique index on a release that refuses one on an existing table.
//
// A global index, synchronous or asynchronous, unique or not, with key and
// covered columns, is created. What a YDB index has no clause for is refused
// in the embedded engine's words. A vector, full-text or local index is
// refused too: the pinned table protocol builds none of them with a table.
func declaredIndexes(table *protocol.DeclaredTable, key []string, columns []*Ydb_Table.ColumnMeta) ([]*Ydb_Table.TableIndex, error) {
	types := make(map[string]string, len(columns))
	for _, column := range columns {
		name, _, err := schema.ColumnType(column.GetType())
		if err != nil {
			return nil, err
		}
		types[column.GetName()] = name
	}
	var result []*Ydb_Table.TableIndex
	for _, index := range table.GetIndexes() {
		compiled, err := declaredIndex(table, index, key, types)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(result, func(other *Ydb_Table.TableIndex) bool { return other.GetName() == compiled.GetName() }) {
			return nil, unsupported("table %q: two of its indexes are named %q, and YDB names an index once per table", table.GetName(), compiled.GetName())
		}
		result = append(result, compiled)
	}
	return result, nil
}

func declaredIndex(declared *protocol.DeclaredTable, index *protocol.DeclaredIndex, key []string, types map[string]string) (*Ydb_Table.TableIndex, error) {
	table := declared.GetName()
	if err := only(index, "name", "fields", "parts", "unique", "type", "include_columns", "table_name", "requires", "concurrently"); err != nil {
		return nil, err
	}
	subject := fmt.Sprintf("index %q", index.GetName())
	refuse := func(reason string) error { return unsupported("%s: %s", subject, reason) }
	if strings.TrimSpace(index.GetName()) == "" {
		return nil, unsupported("an index on table %q: YDB names every index, and this one has no name", table)
	}
	if !validName(index.GetName()) {
		return nil, fmt.Errorf("invalid YDB index name")
	}
	if !namesTable(index.GetTableName(), declared) {
		return nil, refuse(fmt.Sprintf("it names table %q, and the table declaring it is %q", index.GetTableName(), table))
	}
	kind, err := ydbdecl.IndexKindOf(index.GetType())
	if err != nil {
		return nil, refuse(err.Error())
	}
	switch {
	case kind != ydbdecl.IndexSync && kind != ydbdecl.IndexAsync:
		return nil, refuse(fmt.Sprintf("it is a %s index, and this provider creates a table with its global indexes, synchronous or asynchronous", kind))
	case kind == ydbdecl.IndexAsync && index.GetUnique():
		return nil, refuse("a unique index is synchronous on YDB (`GLOBAL UNIQUE ASYNC` answers `unique: alternative is not implemented yet`)")
	}
	keys, err := indexKeyColumns(subject, index)
	if err != nil {
		return nil, err
	}
	cover := slices.Clone(index.GetIncludeColumns())
	for i, covered := range cover {
		switch {
		case slices.Contains(keys, covered):
			return nil, refuse(fmt.Sprintf("column %q is both a key and a covered column, which YDB refuses", covered))
		case slices.Contains(cover[:i], covered):
			return nil, refuse(fmt.Sprintf("it covers column %q twice", covered))
		case slices.Contains(key, covered):
			return nil, refuse(fmt.Sprintf("it covers key column %q, which every YDB index carries already", covered))
		}
		if _, declared := types[covered]; !declared {
			return nil, refuse(fmt.Sprintf("it covers column %q, which the table does not declare", covered))
		}
	}
	if slices.Equal(keys, key) {
		return nil, refuse("its columns are the table's key, which YDB refuses (`index keys shouldn't be table keys`)")
	}
	for i, name := range keys {
		ydbType, declared := types[name]
		switch {
		case !declared:
			return nil, refuse(fmt.Sprintf("it names column %q, which the table does not declare", name))
		case slices.Contains(keys[:i], name):
			return nil, refuse(fmt.Sprintf("it names column %q twice", name))
		case !keyComparable(ydbType):
			return nil, refuse(fmt.Sprintf("column %q is %s, which YDB refuses as an index key", name, ydbType))
		}
	}
	result := &Ydb_Table.TableIndex{Name: index.GetName(), IndexColumns: keys, DataColumns: cover}
	switch {
	case kind == ydbdecl.IndexAsync:
		result.Type = &Ydb_Table.TableIndex_GlobalAsyncIndex{GlobalAsyncIndex: &Ydb_Table.GlobalAsyncIndex{}}
	case index.GetUnique():
		result.Type = &Ydb_Table.TableIndex_GlobalUniqueIndex{GlobalUniqueIndex: &Ydb_Table.GlobalUniqueIndex{}}
	default:
		result.Type = &Ydb_Table.TableIndex_GlobalIndex{GlobalIndex: &Ydb_Table.GlobalIndex{}}
	}
	return result, nil
}

// namesTable reports whether an index's table name is the declared table:
// empty, its name, or its name qualified with its directory, as the host
// qualifies it with a dot.
func namesTable(name string, table *protocol.DeclaredTable) bool {
	return name == "" || name == table.GetName() ||
		(table.GetSchema() != "" && (name == table.GetSchema()+"."+table.GetName() || name == table.GetSchema()+"/"+table.GetName()))
}

// indexKeyColumns are an index's key columns. A YDB index key is a list of
// columns: an expression, an order, a prefix, a NULLS order or an operator
// class has no spelling.
func indexKeyColumns(subject string, index *protocol.DeclaredIndex) ([]string, error) {
	if len(index.GetParts()) == 0 {
		if len(index.GetFields()) == 0 {
			return nil, unsupported("%s: the index names no column", subject)
		}
		return slices.Clone(index.GetFields()), nil
	}
	columns := make([]string, 0, len(index.GetParts()))
	for _, part := range index.GetParts() {
		switch {
		case part.GetExpression() != "":
			return nil, unsupported("%s: YDB has no expression index", subject)
		case part.GetDescending():
			return nil, unsupported("%s: a YDB index column has no order", subject)
		case part.GetPrefix() != "":
			return nil, unsupported("%s: a YDB index column has no prefix length", subject)
		case part.GetNulls() != "":
			return nil, unsupported("%s: a YDB index column has no NULLS order", subject)
		case part.GetOperatorClass() != "":
			return nil, unsupported("%s: YDB has no index operator class", subject)
		}
		columns = append(columns, part.GetColumn())
	}
	if len(index.GetFields()) != 0 && !slices.Equal(index.GetFields(), columns) {
		return nil, fmt.Errorf("the parts of YDB index %q disagree with its columns", index.GetName())
	}
	return columns, nil
}

// keyComparable reports a type YDB takes in a key: every type but the
// floating-point, JSON and YSON ones.
func keyComparable(ydbType string) bool {
	switch ydbType {
	case "Float", "Double", "Json", "JsonDocument", "Yson":
		return false
	default:
		return true
	}
}

// validateIndexes refuses native creation indexes this provider does not
// build: one of another kind, with settings of its own, or that reads a column
// the request does not create, and two indexes of one name.
func validateIndexes(request *Ydb_Table.CreateTableRequest, names map[string]bool) error {
	seen := map[string]bool{}
	for _, index := range request.GetIndexes() {
		if err := only(index, "name", "index_columns", "data_columns", "global_index", "global_async_index", "global_unique_index"); err != nil {
			return err
		}
		if !validName(index.GetName()) || seen[index.GetName()] || len(index.GetIndexColumns()) == 0 {
			return fmt.Errorf("invalid or duplicate YDB creation index")
		}
		seen[index.GetName()] = true
		for _, inner := range []proto.Message{index.GetGlobalIndex(), index.GetGlobalAsyncIndex(), index.GetGlobalUniqueIndex()} {
			if inner != nil && proto.Size(inner) != 0 {
				return fmt.Errorf("a YDB creation index states no settings of its own")
			}
		}
		columns := slices.Concat(index.GetIndexColumns(), index.GetDataColumns())
		for i, column := range columns {
			if !names[column] || slices.Contains(columns[:i], column) {
				return fmt.Errorf("a YDB creation index reads each declared column once")
			}
		}
	}
	return nil
}
