package backend

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func normalizeOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	switch input.GetValue().GetKind() {
	case schema.CoordinationOperation:
		return schema.NormalizeCoordinationOperation(input)
	case tablecreate.OperationKind:
		return tablecreate.Normalize(input)
	case tablecreate.AlterOperationKind:
		return tablecreate.NormalizeAlteration(input)
	case tablecreate.ColumnOperationKind:
		return tablecreate.NormalizeColumnTable(input)
	case tablecreate.ColumnTTLOperationKind:
		return tablecreate.NormalizeColumnTTLAlteration(input)
	default:
		return nil, fmt.Errorf("unsupported YDB native operation")
	}
}
