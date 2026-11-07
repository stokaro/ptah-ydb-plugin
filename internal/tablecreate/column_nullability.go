package tablecreate

import (
	"encoding/json"
	"fmt"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// NullabilityOperationKind identifies the change of one NOT NULL column of a
// row table to one that takes NULL.
const NullabilityOperationKind = schema.ProviderID + "/column-nullability-operation"

var nullabilityModel = buildStatementModel(NullabilityOperationKind, map[string]any{
	"payload":      "held: the complete native column observation of a NOT NULL column",
	"execution":    "one YQL ALTER TABLE ALTER COLUMN DROP NOT NULL through the query service, outside transactions, without retries",
	"precondition": "the owner is a row table and the column matches the captured name, storage type, NOT NULL and every native setting",
	"confirmation": "the column takes NULL after the statement; the key and every other column definition remain unchanged",
	"semantics":    "behavioral: the column takes NULL from now on; the rows keep their values",
})

// NullabilityModel is the model of a column nullability operation.
func NullabilityModel() *protocol.ValueModel { return proto.CloneOf(nullabilityModel) }

// NullabilityAlteration is a NOT NULL column, as the table held it, that is
// to take NULL.
type NullabilityAlteration struct {
	Held *native.ColumnMeta
}

type nullabilityPayload struct {
	Held json.RawMessage `json:"held"`
}

func (p nullabilityPayload) decode() (*NullabilityAlteration, error) {
	if len(p.Held) == 0 {
		return nil, fmt.Errorf("a column nullability operation requires a captured column")
	}
	result := &NullabilityAlteration{Held: &native.ColumnMeta{}}
	if err := protojson.Unmarshal(p.Held, result.Held); err != nil {
		return nil, err
	}
	return result, nil
}

func (p nullabilityPayload) validate() error {
	change, err := p.decode()
	if err != nil {
		return err
	}
	if err := schema.Known(change.Held); err != nil {
		return err
	}
	if !validName(change.Held.Name) {
		return fmt.Errorf("a column nullability operation requires a named column")
	}
	if _, nullable, err := schema.ColumnType(change.Held.Type); err != nil || nullable {
		return fmt.Errorf("a column nullability operation requires a NOT NULL column of a type YDB stores")
	}
	return nil
}

// CaptureNullability builds one column nullability operation from captured
// state.
func CaptureNullability(id string, owner *protocol.Reference, change *NullabilityAlteration) (*protocol.NormalizedValue, error) {
	if change == nil || change.Held == nil {
		return nil, fmt.Errorf("a column nullability operation requires its before-state")
	}
	held, err := protojson.Marshal(change.Held)
	if err != nil {
		return nil, err
	}
	return captureStatement(id, owner, NullabilityModel(), nullabilityPayload{Held: held}, NormalizeNullability)
}

// NormalizeNullability reconstructs the operation's behavior from validated
// values.
func NormalizeNullability(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	payload, err := modeljson.Canonical(input.GetValue().GetPayload())
	if err != nil {
		return nil, err
	}
	checked := proto.CloneOf(input)
	checked.Value.Payload = payload
	change, err := decodeStatement[nullabilityPayload](checked, NullabilityModel())
	if err != nil {
		return nil, err
	}
	return normalizedStatement(checked, NullabilityModel(), change, &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "Lets a column take NULL without changing existing rows", Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "A column's nullability does not change access policy"})
}

// DecodeNullability validates and decodes a column nullability operation.
func DecodeNullability(value *protocol.Envelope) (*NullabilityAlteration, error) {
	normalized, err := NormalizeNullability(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	payload, err := decodeStatement[nullabilityPayload](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: normalized.Value},
		NullabilityModel())
	if err != nil {
		return nil, err
	}
	return payload.decode()
}

// NullabilityStatement writes the DROP NOT NULL of the column, as the host's
// embedded engine wrote it. YDB takes it on 25.1.4.7 and 26.2.1.14, and the
// column reads as an optional type after it.
func NullabilityStatement(path string, change *NullabilityAlteration) string {
	return "ALTER TABLE " + QuoteIdentifier(path) + " ALTER COLUMN " + QuoteIdentifier(change.Held.Name) + " DROP NOT NULL;"
}
