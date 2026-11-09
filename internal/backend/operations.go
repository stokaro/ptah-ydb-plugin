package backend

import (
	"fmt"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func normalizeOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	switch input.GetValue().GetKind() {
	case schema.SecretOperation:
		return schema.NormalizeSecretOperation(input)
	case declarationplan.OperationKind:
		return declarationplan.Normalize(input)
	case schema.CoordinationOperation:
		return schema.NormalizeCoordinationOperation(input)
	case schema.StreamingQueryOperation:
		return schema.NormalizeStreamingQueryOperation(input)
	case schema.TopicOperation:
		return schema.NormalizeTopicOperation(input)
	case schema.ChangefeedOperation:
		return schema.NormalizeChangefeedOperation(input)
	case schema.ExternalSourceOperation, schema.ExternalTableOperation:
		return schema.NormalizeExternalOperation(input)
	case schema.WorkloadOperation:
		return schema.NormalizeWorkloadOperation(input)
	case schema.ReplicationOperation:
		return schema.NormalizeReplicationOperation(input)
	case schema.TransferOperation:
		return schema.NormalizeTransferOperation(input)
	default:
		return normalizeTableOperation(input)
	}
}

// normalizeTableOperation normalizes an operation on a row or column table,
// and refuses any other kind as unsupported.
func normalizeTableOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	switch input.GetValue().GetKind() {
	case tablecreate.OperationKind:
		return tablecreate.Normalize(input)
	case tablecreate.InventoryOperationKind:
		return tablecreate.NormalizeInventory(input)
	case tablecreate.DefaultOperationKind:
		return tablecreate.NormalizeDefault(input)
	case tablecreate.NullabilityOperationKind:
		return tablecreate.NormalizeNullability(input)
	case tablecreate.AlterOperationKind:
		return tablecreate.NormalizeAlteration(input)
	case tablecreate.IndexSettingsOperationKind:
		return tablecreate.NormalizeIndexSettings(input)
	case tablecreate.ColumnOperationKind:
		return tablecreate.NormalizeColumnTable(input)
	case tablecreate.ColumnTTLOperationKind:
		return tablecreate.NormalizeColumnTTLAlteration(input)
	case tablecreate.ColumnLocalIndexOperationKind:
		return tablecreate.NormalizeColumnLocalIndex(input)
	case tablecreate.DropOperationKind:
		return tablecreate.NormalizeDrop(input)
	case tablecreate.CopyOperationKind:
		return tablecreate.NormalizeCopy(input)
	case tablecreate.RenameOperationKind:
		return tablecreate.NormalizeRename(input)
	case tablecreate.NativeIndexOperationKind:
		return tablecreate.NormalizeNativeIndex(input)
	case tablecreate.SequenceOperationKind:
		return tablecreate.NormalizeSequence(input)
	default:
		return nil, fmt.Errorf("unsupported YDB native operation")
	}
}
