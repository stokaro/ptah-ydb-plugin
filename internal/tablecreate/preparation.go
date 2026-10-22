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
		compiled, err := Compile(owner, input.Desired)
		if err != nil {
			return nil, err
		}
		result.Tables = append(result.Tables, &protocol.PreparedTable{
			Subject: proto.CloneOf(input.Subject), ColumnPrimaryKeysPrepared: true,
			ColumnPrimaryKeys: slices.Clone(compiled.PrimaryKey),
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
