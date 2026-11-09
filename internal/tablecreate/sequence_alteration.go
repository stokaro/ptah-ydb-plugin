package tablecreate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"ptah.run/provider-sdk/modeljson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// SequenceOperationKind identifies the change of the start and the increment
// of the sequence behind a Serial column of an existing row table.
const SequenceOperationKind = schema.ProviderID + "/serial-sequence-operation"

var sequenceModel = buildStatementModel(SequenceOperationKind, map[string]any{
	"payload": "held: the complete native column observation, its sequence included and unrestarted; start and increment: what the sequence takes, each a whole number of at least 1 in decimal text, not both what it holds; " +
		"database: the path of the database the plan read, which the TEXT form names the sequence under when the owner names no database, absent for a plan made against no read",
	"execution":    "one YQL ALTER SEQUENCE ... START WITH ... INCREMENT BY ... on the sequence's absolute path through the query service, outside transactions, without a restart and without retries; the absolute path is bound at dispatch",
	"precondition": "the owner is a row table and the column matches the captured native column, its sequence included; a 16-bit or 32-bit Serial needs serial_sequence_keeps_range",
	"confirmation": "the sequence reads back with the requested start and increment and no restart; the key and every other column remain unchanged",
	"semantics":    "behavioral: rows inserted later take values the new settings give; existing rows and the next value the sequence already computed remain unchanged",
})

// SequenceModel binds the captured column and the settings its sequence takes.
func SequenceModel() *protocol.ValueModel { return proto.CloneOf(sequenceModel) }

// SequenceAlteration is the change of a Serial column's sequence: the column
// as the plan read it, and the start and the increment the sequence takes.
type SequenceAlteration struct {
	Held     *native.ColumnMeta
	Settings ydbsequence.Settings
	// Database is the path of the database the plan read, "" for a plan made
	// against no read. The host names a table it plans by no database, so the
	// TEXT form, which names the sequence by its absolute path, takes it from
	// here.
	Database string
}

type sequencePayload struct {
	Held      json.RawMessage `json:"held"`
	Start     string          `json:"start"`
	Increment string          `json:"increment"`
	Database  string          `json:"database,omitempty"`
}

func (p sequencePayload) decode() (*SequenceAlteration, error) {
	if len(p.Held) == 0 || bytes.Equal(p.Held, []byte("null")) {
		return nil, fmt.Errorf("a sequence operation requires a captured column")
	}
	if p.Database != "" && (!path.IsAbs(p.Database) || path.Clean(p.Database) != p.Database) {
		return nil, fmt.Errorf("a sequence operation names its database by a clean absolute path")
	}
	result := &SequenceAlteration{Held: &native.ColumnMeta{}, Database: p.Database}
	if err := protojson.Unmarshal(p.Held, result.Held); err != nil {
		return nil, err
	}
	for _, setting := range []struct {
		text   string
		target *int64
	}{{p.Start, &result.Settings.Start}, {p.Increment, &result.Settings.Increment}} {
		value, err := strconv.ParseInt(setting.text, 10, 64)
		if err != nil || value < 1 || strconv.FormatInt(value, 10) != setting.text {
			return nil, fmt.Errorf("a sequence operation states its start and increment as whole numbers of at least 1")
		}
		*setting.target = value
	}
	return result, nil
}

func (p sequencePayload) validate() error {
	change, err := p.decode()
	if err != nil {
		return err
	}
	if err := schema.Known(change.Held); err != nil {
		return err
	}
	if !validName(change.Held.GetName()) || HeldSerialType(change.Held) == "" {
		return fmt.Errorf("a sequence operation requires a named Serial column")
	}
	held := change.Held.GetFromSequence()
	if held.GetSetVal() != nil {
		return fmt.Errorf("a sequence operation does not change a sequence some ALTER SEQUENCE restarted")
	}
	if heldSettings(held) == change.Settings {
		return fmt.Errorf("a sequence operation must change the sequence")
	}
	return nil
}

// HeldSerialType is the Serial type of a column a read describes, from the
// integer type its sequence fills, or "" for a column without a sequence.
func HeldSerialType(column *native.ColumnMeta) string {
	if column.GetFromSequence() == nil {
		return ""
	}
	spelling, nullable, err := schema.ColumnType(column.GetType())
	if err != nil || (nullable && !column.GetNotNull()) {
		return ""
	}
	serial, _ := ydbsequence.SerialFor(spelling)
	return serial
}

// heldSettings is the start and the increment a read reports of a sequence,
// a setting it leaves out read as 1.
func heldSettings(sequence *native.SequenceDescription) ydbsequence.Settings {
	settings := ydbsequence.Default
	if sequence.StartValue != nil {
		settings.Start = sequence.GetStartValue()
	}
	if sequence.Increment != nil {
		settings.Increment = sequence.GetIncrement()
	}
	return settings
}

// CaptureSequence builds the change of a Serial column's sequence from the
// column the plan read.
func CaptureSequence(id string, owner *protocol.Reference, change *SequenceAlteration) (*protocol.NormalizedValue, error) {
	if change == nil || change.Held == nil {
		return nil, fmt.Errorf("a sequence operation requires its before-state")
	}
	held, err := protojson.Marshal(change.Held)
	if err != nil {
		return nil, err
	}
	payload := sequencePayload{Held: held, Start: strconv.FormatInt(change.Settings.Start, 10), Increment: strconv.FormatInt(change.Settings.Increment, 10), Database: change.Database}
	return captureStatement(id, owner, SequenceModel(), payload, NormalizeSequence)
}

// NormalizeSequence reconstructs the operation's behavior from validated
// values.
func NormalizeSequence(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	payload, err := modeljson.Canonical(input.GetValue().GetPayload())
	if err != nil {
		return nil, err
	}
	checked := proto.CloneOf(input)
	checked.Value.Payload = payload
	change, err := decodeStatement[sequencePayload](checked, SequenceModel())
	if err != nil {
		return nil, err
	}
	return normalizedStatement(checked, SequenceModel(), change, &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "Changes the start and the increment of a Serial column's sequence for rows inserted later", Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "A sequence's settings do not change access policy"})
}

// DecodeSequence validates and decodes the change of a Serial column's
// sequence.
func DecodeSequence(value *protocol.Envelope) (*SequenceAlteration, error) {
	normalized, err := NormalizeSequence(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	payload, err := decodeStatement[sequencePayload](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: normalized.Value}, SequenceModel())
	if err != nil {
		return nil, err
	}
	return payload.decode()
}

// SequenceAlterationStatement is the ALTER SEQUENCE that changes the sequence
// of a Serial column of the table at table, in the database at database, or in
// the one the plan read where database is "", as the host's embedded engine
// writes it for a table that exists: no restart, since a restart onto a value
// a row holds fails the next insert. The statement names the sequence by its
// absolute path, so a change in no named database has no text form.
func SequenceAlterationStatement(database, table string, change *SequenceAlteration) (string, error) {
	database = cmp.Or(database, change.Database)
	if database == "" {
		return "", noTextForm("the sequence of column %q changes through an ALTER SEQUENCE, which names the sequence by its absolute path, and the change names no database", change.Held.GetName())
	}
	return SequenceStatement(ydbsequence.Path(database, "", table, change.Held.GetName()), change.Settings), nil
}
