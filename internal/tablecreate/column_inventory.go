package tablecreate

import (
	"bytes"
	"encoding/json"
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// InventoryOperationKind identifies one captured column addition or removal.
const InventoryOperationKind = schema.ProviderID + "/column-inventory-operation"

var inventoryModel = buildStatementModel(InventoryOperationKind, map[string]any{
	"payload":      "exactly one of add (a nullable column without a default or family) and drop (the complete effective captured column definition)",
	"execution":    "one native AlterTable outside transactions, without retries",
	"precondition": "the row table exists; an addition is absent; a removal matches its captured column and has no key, index, TTL, or partitioning dependency",
	"confirmation": "the requested column is added or removed, every other column and all table settings remain unchanged",
	"semantics":    "add is additive; drop destroys the column and its data",
})

// InventoryModel binds complete column inventory operands and their lifecycle.
func InventoryModel() *protocol.ValueModel { return proto.CloneOf(inventoryModel) }

// ColumnInventory contains one addition or one captured removal.
type ColumnInventory struct {
	Add  *Ydb_Table.ColumnMeta
	Drop *native.ColumnMeta
}
type inventoryPayload struct {
	Add  json.RawMessage `json:"add,omitempty"`
	Drop json.RawMessage `json:"drop,omitempty"`
}

func (p inventoryPayload) decode() (*ColumnInventory, error) {
	if (len(p.Add) == 0) == (len(p.Drop) == 0) || bytes.Equal(p.Add, []byte("null")) || bytes.Equal(p.Drop, []byte("null")) {
		return nil, fmt.Errorf("column inventory requires exactly one non-null operand")
	}
	result := &ColumnInventory{}
	if len(p.Add) != 0 {
		result.Add = &Ydb_Table.ColumnMeta{}
		if err := protojson.Unmarshal(p.Add, result.Add); err != nil {
			return nil, err
		}
	}
	if len(p.Drop) != 0 {
		result.Drop = &native.ColumnMeta{}
		if err := protojson.Unmarshal(p.Drop, result.Drop); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (p inventoryPayload) validate() error {
	change, err := p.decode()
	if err != nil {
		return err
	}
	if change.Add != nil {
		return validateColumnAlteration(&Ydb_Table.AlterTableRequest{AddColumns: []*Ydb_Table.ColumnMeta{change.Add}})
	}
	if !validName(change.Drop.Name) {
		return fmt.Errorf("a removed column needs its captured name")
	}
	_, err = defaultColumnState(change.Drop)
	return err
}

// CaptureInventory captures a single column addition or removal.
func CaptureInventory(id string, owner *protocol.Reference, change *ColumnInventory) (*protocol.NormalizedValue, error) {
	if change == nil {
		return nil, fmt.Errorf("column inventory requires its operands")
	}
	payload := inventoryPayload{}
	var err error
	if change.Add != nil {
		payload.Add, err = protojson.Marshal(change.Add)
		if err != nil {
			return nil, err
		}
	}
	if change.Drop != nil {
		payload.Drop, err = protojson.Marshal(change.Drop)
		if err != nil {
			return nil, err
		}
	}
	return captureStatement(id, owner, InventoryModel(), payload, NormalizeInventory)
}

// NormalizeInventory derives destructive or additive facts from the operand.
func NormalizeInventory(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	payload, err := decodeStatement[inventoryPayload](input, InventoryModel())
	if err != nil {
		return nil, err
	}
	impact, reason := protocol.ChangeSemantics_ADDITIVE, "Adds one nullable column"
	if len(payload.Drop) != 0 {
		impact, reason = protocol.ChangeSemantics_DESTRUCTIVE, "Drops the captured column and its data"
	}
	return normalizedStatement(input, InventoryModel(), payload, &protocol.OperationSemantics{Impact: impact, Reason: reason, Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Column inventory does not change access policy"})
}

// DecodeInventory validates and decodes one inventory operation.
func DecodeInventory(value *protocol.Envelope) (*ColumnInventory, error) {
	payload, err := decodeStatement[inventoryPayload](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value}, InventoryModel())
	if err != nil {
		return nil, err
	}
	return payload.decode()
}

// Request creates independent native operands without a session or path.
func (c *ColumnInventory) Request() *Ydb_Table.AlterTableRequest {
	if c.Add != nil {
		return &Ydb_Table.AlterTableRequest{AddColumns: []*Ydb_Table.ColumnMeta{proto.CloneOf(c.Add)}}
	}
	return &Ydb_Table.AlterTableRequest{DropColumns: []string{c.Drop.Name}}
}

// MatchesDrop compares every native setting and effective common column fact.
func (c *ColumnInventory) MatchesDrop(column *native.ColumnMeta) (bool, error) {
	return EqualCapturedColumns(c.Drop, column)
}

// InventoryStatement renders one column inventory operation for review.
func InventoryStatement(path string, change *ColumnInventory) (string, error) {
	prefix := "ALTER TABLE " + QuoteIdentifier(path)
	if change.Drop != nil {
		return prefix + " DROP COLUMN " + QuoteIdentifier(change.Drop.Name) + ";", nil
	}
	typ, _, err := schema.ColumnType(change.Add.Type)
	if err != nil {
		return "", err
	}
	return prefix + " ADD COLUMN " + QuoteIdentifier(change.Add.Name) + " " + typ + ";", nil
}
