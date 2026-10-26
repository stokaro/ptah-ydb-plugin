package tablecreate

import (
	"context"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// PrepareTables derives key membership from the same captured native declaration
// that CREATE consumes. It does not inspect a server or replace authored values.
// Declarations outside the implemented row-table contract are refused as a batch.
func PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	if err := validateTableIdentifiers(request.Identifiers); err != nil {
		return nil, err
	}
	result := &protocol.PreparedTables{Complete: true}
	for _, input := range request.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner, err := boundOwner(input.Subject)
		if err != nil {
			return nil, err
		}
		key, err := compiledKey(owner, input.Desired, request.Capabilities)
		if err != nil {
			return nil, err
		}
		result.Tables = append(result.Tables, &protocol.PreparedTable{
			Subject: proto.CloneOf(input.Subject), ColumnPrimaryKeysPrepared: true,
			ColumnPrimaryKeys: key,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// compiledKey returns the ordered key the CREATE of a declaration writes, a
// row table's or a column table's.
func compiledKey(owner *protocol.Reference, table *protocol.DeclaredTable, caps map[string]bool) ([]string, error) {
	if IsColumnTable(table) {
		created, err := CompileColumnTable(owner, table, caps)
		if err != nil {
			return nil, err
		}
		return slices.Clone(created.PrimaryKey), nil
	}
	compiled, err := Compile(owner, table, caps)
	if err != nil {
		return nil, err
	}
	return slices.Clone(compiled.PrimaryKey), nil
}
