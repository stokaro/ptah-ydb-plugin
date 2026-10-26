package tablecreate

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// DefaultOperationKind identifies one presence-aware column default change.
const DefaultOperationKind = schema.ProviderID + "/column-default-operation"

var defaultModel = buildStatementModel(DefaultOperationKind, map[string]any{
	"payload":      "held: the complete native column observation; set: a typed literal, omitted to drop the default; explicit null is invalid",
	"execution":    "one YQL ALTER TABLE ALTER COLUMN SET DEFAULT or DROP DEFAULT through the query service, outside transactions, without retries",
	"precondition": "the owner is a row table and the column matches the captured name, effective storage type and nullability, and all native settings; a sequence default is refused; the new literal has the column's storage type",
	"confirmation": "the default equals the requested literal or is absent after drop; the key and every other column definition remain unchanged",
	"semantics":    "behavioral: future inserted rows use the new default; existing rows remain unchanged",
})

// DefaultModel binds the captured before-state and optional typed replacement.
func DefaultModel() *protocol.ValueModel { return proto.CloneOf(defaultModel) }

// DefaultAlteration retains a column's before-state and the requested default.
// Set is nil only for a requested removal; an empty literal is a present value.
type DefaultAlteration struct {
	Held *native.ColumnMeta
	Set  *Ydb.TypedValue
}

type defaultPayload struct {
	Held json.RawMessage `json:"held"`
	Set  json.RawMessage `json:"set,omitempty"`
}

func (p defaultPayload) decode() (*DefaultAlteration, error) {
	if len(p.Held) == 0 || bytes.Equal(p.Held, []byte("null")) || bytes.Equal(p.Set, []byte("null")) {
		return nil, fmt.Errorf("a column default operation requires a captured column and no null values")
	}
	result := &DefaultAlteration{Held: &native.ColumnMeta{}}
	if err := protojson.Unmarshal(p.Held, result.Held); err != nil {
		return nil, err
	}
	if len(p.Set) != 0 {
		result.Set = &Ydb.TypedValue{}
		if err := protojson.Unmarshal(p.Set, result.Set); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (p defaultPayload) validate() error {
	change, err := p.decode()
	if err != nil {
		return err
	}
	if err := schema.Known(change.Held); err != nil {
		return err
	}
	if !validName(change.Held.Name) || change.Held.GetFromSequence() != nil {
		return fmt.Errorf("a column default operation requires a named non-sequence column")
	}
	kind, _, err := schema.ColumnType(change.Held.Type)
	if err != nil {
		return err
	}
	held, err := defaultText(change.Held.GetFromLiteral())
	if err != nil {
		return err
	}
	set, err := defaultText(change.Set)
	if err != nil {
		return err
	}
	if change.Set != nil {
		next, _, err := schema.ColumnType(change.Set.Type)
		if err != nil {
			return err
		}
		if next != kind {
			return fmt.Errorf("a column default replacement has a different storage type")
		}
	}
	if held == set {
		return fmt.Errorf("a column default operation must change the default")
	}
	return nil
}

func defaultText(value *Ydb.TypedValue) (string, error) {
	if value == nil {
		return "", nil
	}
	return columnsemantics.Literal(value)
}

// CaptureDefault builds one column default operation from captured state.
func CaptureDefault(id string, owner *protocol.Reference, change *DefaultAlteration) (*protocol.NormalizedValue, error) {
	if change == nil || change.Held == nil {
		return nil, fmt.Errorf("a column default operation requires its before-state")
	}
	held, err := protojson.Marshal(change.Held)
	if err != nil {
		return nil, err
	}
	payload := defaultPayload{Held: held}
	if change.Set != nil {
		payload.Set, err = protojson.Marshal(change.Set)
		if err != nil {
			return nil, err
		}
	}
	return captureStatement(id, owner, DefaultModel(), payload, NormalizeDefault)
}

// NormalizeDefault reconstructs the operation's behavior from validated values.
func NormalizeDefault(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	payload, err := modeljson.Canonical(input.GetValue().GetPayload())
	if err != nil {
		return nil, err
	}
	checked := proto.CloneOf(input)
	checked.Value.Payload = payload
	change, err := decodeStatement[defaultPayload](checked, DefaultModel())
	if err != nil {
		return nil, err
	}
	return normalizedStatement(checked, DefaultModel(), change, &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "Changes the default used for future inserted rows without changing existing rows", Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Column defaults do not change access policy"})
}

// DecodeDefault validates and decodes a column default operation.
func DecodeDefault(value *protocol.Envelope) (*DefaultAlteration, error) {
	normalized, err := NormalizeDefault(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	payload, err := decodeStatement[defaultPayload](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: normalized.Value}, DefaultModel())
	if err != nil {
		return nil, err
	}
	return payload.decode()
}

// DefaultStatement writes the complete typed SET or presence-aware DROP.
func DefaultStatement(path string, change *DefaultAlteration) (string, error) {
	prefix := "ALTER TABLE " + QuoteIdentifier(path) + " ALTER COLUMN " + QuoteIdentifier(change.Held.Name)
	if change.Set == nil {
		return prefix + " DROP DEFAULT;", nil
	}
	literal, err := columnsemantics.Literal(change.Set)
	if err != nil {
		return "", err
	}
	return prefix + " SET DEFAULT " + literal + ";", nil
}

// MatchesHeld compares common column facts and every captured native setting.
// Native descriptions can express the same nullability through an optional
// type or NotNull; inspection publishes their effective value once.
func (c *DefaultAlteration) MatchesHeld(column *native.ColumnMeta) (bool, error) {
	return EqualCapturedColumns(c.Held, column)
}

// EqualCapturedColumns compares native settings and effective common facts.
func EqualCapturedColumns(heldColumn, actualColumn *native.ColumnMeta) (bool, error) {
	held, err := defaultColumnState(heldColumn)
	if err != nil {
		return false, err
	}
	actual, err := defaultColumnState(actualColumn)
	if err != nil {
		return false, err
	}
	return proto.Equal(held, actual), nil
}

func defaultColumnState(column *native.ColumnMeta) (*native.ColumnMeta, error) {
	if err := schema.Known(column); err != nil {
		return nil, err
	}
	name, nullable, err := schema.ColumnType(column.GetType())
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(column)
	result.Type, err = schema.StorageType(name, nullable && !column.GetNotNull())
	if err != nil {
		return nil, err
	}
	result.NotNull = nil
	if result.Family == "" {
		result.Family = DefaultFamily
	}
	return result, nil
}
