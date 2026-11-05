package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func (s *source) readIndexes(ctx context.Context, tablePath string, table *description) (*native.IndexObservations, error) {
	result := &native.IndexObservations{}
	columns := make(map[string]bool)
	for _, column := range table.table.GetColumns() {
		columns[column.GetName()] = true
	}
	for _, index := range table.indexes {
		for _, names := range [][]string{index.IndexColumns, index.DataColumns} {
			for _, name := range names {
				if !columns[name] {
					return nil, fmt.Errorf("YDB index names a missing table column")
				}
			}
		}
		observed := &native.ObservedIndex{Description: index}
		names, err := schema.ImplementationNames(index)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			implementation, err := s.describeTable(ctx, path.Join(tablePath, index.Name, name))
			if err != nil {
				return nil, fmt.Errorf("read YDB index implementation %s: %w", name, err)
			}
			if len(implementation.indexes) != 0 || len(implementation.changefeeds) != 0 {
				return nil, fmt.Errorf("nested child objects on a YDB index implementation table")
			}
			if implementation.table.GetSelf().GetName() != name || implementation.table.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE {
				return nil, fmt.Errorf("YDB index implementation identity changed")
			}
			observed.Tables = append(observed.Tables, &native.ImplementationTable{Name: name, Description: implementation.table})
		}
		result.Indexes = append(result.Indexes, observed)
	}
	return result, nil
}

// commonIndexes are the indexes of a row table the common model describes:
// the global secondary indexes, synchronous, asynchronous and unique, by
// their key and covered columns. A vector or full-text index has settings
// the common model has no place for, so it stays in the native index facet
// alone, where the host can neither create nor drop it. An index's method is
// its kind's common spelling ([ydbdecl.IndexKind.CommonMethod]), the one table
// preparation states for a declared index: "async" for an asynchronous index
// and none for a synchronous one. The comment travels on the common-object
// wire.
func commonIndexes(table *protocol.Reference, observed *native.IndexObservations) []*protocol.ObservedIndex {
	var result []*protocol.ObservedIndex
	for _, index := range observed.GetIndexes() {
		description := index.GetDescription()
		common := &protocol.ObservedIndex{Name: description.GetName(), TableName: table.GetName(), Schema: table.GetSchema(),
			Columns: slices.Clone(description.GetIndexColumns()), IncludeColumns: slices.Clone(description.GetDataColumns())}
		if !schema.CommonIndex(description) {
			continue
		}
		kind := ydbdecl.IndexSync
		if description.GetGlobalAsyncIndex() != nil {
			kind = ydbdecl.IndexAsync
		}
		// A common index is global, synchronous or asynchronous, and either
		// kind has a common spelling.
		common.Method, _ = kind.CommonMethod()
		common.Unique = description.GetGlobalUniqueIndex() != nil
		result = append(result, common)
	}
	return result
}

// nativeIndexes are the indexes of observed the common model cannot describe,
// with the description of each one it describes: the common model has no
// place for an index's partitioning and read replicas, which a rebuild gives
// the new table's index.
func nativeIndexes(observed *native.IndexObservations) *native.IndexObservations {
	result := &native.IndexObservations{}
	for _, index := range observed.GetIndexes() {
		if !schema.CommonIndex(index.GetDescription()) {
			result.Indexes = append(result.Indexes, index)
			continue
		}
		result.CommonIndexes = append(result.CommonIndexes, index.GetDescription())
	}
	return result
}
