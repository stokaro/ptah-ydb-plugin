package inspect

import (
	"context"
	"fmt"
	"path"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

func (s *source) readIndexes(ctx context.Context, tablePath string, table *description) (*native.IndexObservations, error) {
	result := &native.IndexObservations{}
	columns := map[string]bool{}
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
			if len(implementation.indexes) != 0 {
				return nil, fmt.Errorf("nested indexes on a YDB index implementation table")
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
