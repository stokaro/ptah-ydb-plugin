package tablecreate

import (
	"context"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// PrepareTables derives key membership from the same captured native declaration
// that CREATE consumes. It does not inspect a server or replace authored values.
// Declarations outside the implemented row-table contract are refused as a batch.
// A request whose target features list schema.index-methods also gets the kind
// of each declared index, as the CREATE builds it and spelled as inspection
// reports a held index's method.
func PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	if err := validateTableIdentifiers(request.Identifiers); err != nil {
		return nil, err
	}
	methods := slices.Contains(request.GetTargetFeatures(), provider.IndexMethodsFeature)
	result := &protocol.PreparedTables{Complete: true}
	for _, input := range request.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner, err := boundOwner(input.Subject)
		if err != nil {
			return nil, err
		}
		key, indexes, err := compiledTable(owner, input.Desired, request.Capabilities)
		if err != nil {
			return nil, declarationRefusal(owner, err)
		}
		prepared := &protocol.PreparedTable{
			Subject: proto.CloneOf(input.Subject), ColumnPrimaryKeysPrepared: true,
			ColumnPrimaryKeys: key,
		}
		if methods {
			prepared.Indexes = indexes
		}
		result.Tables = append(result.Tables, prepared)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// compiledTable returns the ordered key the CREATE of a declaration writes, a
// row table's or a column table's, and the kind of each index the CREATE
// builds with the table, in the order the table declares them. A column table
// declares no common index.
func compiledTable(owner *protocol.Reference, table *protocol.DeclaredTable, caps map[string]bool) ([]string, []*protocol.PreparedIndex, error) {
	if IsColumnTable(table) {
		created, err := CompileColumnTable(owner, table, caps)
		if err != nil {
			return nil, nil, err
		}
		return slices.Clone(created.PrimaryKey), nil, nil
	}
	compiled, err := predictedCreation(owner, table, caps)
	if err != nil {
		return nil, nil, err
	}
	indexes := make([]*protocol.PreparedIndex, 0, len(compiled.GetIndexes()))
	for _, index := range compiled.GetIndexes() {
		kind := ydbdecl.IndexSync
		if index.GetGlobalAsyncIndex() != nil {
			kind = ydbdecl.IndexAsync
		}
		// A row table builds its global indexes only, each of a kind with a
		// common spelling.
		method, _ := kind.CommonMethod()
		indexes = append(indexes, &protocol.PreparedIndex{Name: index.GetName(), Method: method})
	}
	return slices.Clone(compiled.PrimaryKey), indexes, nil
}
