package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// readColumnTable reads a column table from the table service and the
// monitoring endpoint, and returns it as a common table with its columns, one
// column-storage facet and one facet for each column. It carries no
// row-table, index or changefeed facet: a column table has no row-table
// settings, its local indexes sit in the column-storage facet, and it holds no
// global index or changefeed. A column table is never read without the
// monitoring description, and never as a row table.
func (s *source) readColumnTable(ctx context.Context, columns ColumnTables, database, directory, name string) (*protocol.Table, error) {
	absolute := path.Join(database, directory, name)
	if columns == nil {
		return nil, fmt.Errorf("column table %s requires monitoring=http(s)://host:port in the YDB address: "+
			"the table service leaves out a column table's shard count and local indexes", absolute)
	}
	described, err := s.owner.DescribeColumnTable(ctx, absolute)
	if err != nil {
		return nil, err
	}
	body, err := columns(ctx, absolute)
	if err != nil {
		return nil, fmt.Errorf("column table %s: %w", absolute, err)
	}
	storage, err := columnstore.Decode(body, absolute, described)
	if err != nil {
		return nil, fmt.Errorf("column table %s: %w", absolute, err)
	}
	ref := &protocol.Reference{Engine: "ydb", Database: database, Schema: directory, Kind: "table", Name: name}
	table := &protocol.Table{Identity: ref, NormalizedIdentity: proto.CloneOf(ref)}
	facet, err := schema.CaptureColumnStorage(ref, storage)
	if err != nil {
		return nil, err
	}
	table.Facets = []*protocol.Envelope{facet}
	for position, column := range described.GetColumns() {
		observed, err := columnsemantics.Observed(column, position+1, slices.Contains(described.GetPrimaryKey(), column.GetName()))
		if err != nil {
			return nil, err
		}
		projected := &protocol.Column{Name: column.GetName(), Observed: observed}
		facet, err := schema.CaptureColumn(provider.ColumnReference(ref, column.GetName()), column)
		if err != nil {
			return nil, err
		}
		projected.Facets = []*protocol.Envelope{facet}
		table.Columns = append(table.Columns, projected)
	}
	return table, nil
}
