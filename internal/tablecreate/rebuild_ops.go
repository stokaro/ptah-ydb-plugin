package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// The operations a table rebuild takes beside a creation and a removal: the
// copy of the rows into the new table, and a rename of a table in its
// directory.
const (
	CopyOperationKind   = schema.ProviderID + "/row-table-copy-operation"
	RenameOperationKind = schema.ProviderID + "/row-table-rename-operation"
)

var (
	copyModel = buildStatementModel(CopyOperationKind, map[string]any{
		"payload":      "source: the table rows are read from, in the owner's directory; columns: each column the new table takes from it, with the type it is converted to and the message a row that does not convert or is NULL for a NOT NULL column fails the copy with",
		"execution":    "one YQL INSERT ... SELECT through the query service, outside SQL transactions, which commits whole or not at all; no retries",
		"precondition": "the source must hold every copied column and the owner must exist and hold no row",
		"confirmation": "the statement commits; rows written to the source after it ran are not in the owner",
		"semantics":    "additive: the source keeps its rows",
	})
	renameModel = buildStatementModel(RenameOperationKind, map[string]any{
		"payload":      "target: the name the owner takes in its directory; columns and key: what the owner holds when it is planned",
		"execution":    "YQL ALTER TABLE ... RENAME TO through the query service once, outside SQL transactions; no retries",
		"precondition": "the owner must hold a row table with the captured columns, in order, and the captured key columns, and the target path must be free",
		"confirmation": "the owner's path is free and the target holds the table",
		"semantics":    "behavioral: the table answers to another name",
	})
)

// CopyModel binds the copy payload grammar, its precondition and its
// confirmation.
func CopyModel() *protocol.ValueModel { return proto.CloneOf(copyModel) }

// RenameModel binds the rename payload grammar, its precondition and its
// confirmation.
func RenameModel() *protocol.ValueModel { return proto.CloneOf(renameModel) }

func buildStatementModel(kind string, contract map[string]any) *protocol.ValueModel {
	definition := map[string]any{"kind": kind, "representation": "operation", "schema_version": 1, "encoding": "json",
		"database": "an explicit owner database must match the selected session; an empty database binds to that session"}
	for key, value := range contract {
		definition[key] = value
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: kind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: kind + "/json-v1"}
}

// CopiedColumn is one column a copy writes into the new table.
type CopiedColumn struct {
	Name string `json:"name"`
	// Cast is the YDB type the old value is converted to, empty to copy it as
	// it is.
	Cast string `json:"cast,omitempty"`
	// Required reports a column the new table holds NOT NULL: a NULL, or a
	// value that does not convert, fails the copy.
	Required bool `json:"required,omitempty"`
}

// Copy is a decoded copy of the rows of Source, a table in the owner's
// directory, into the owner.
type Copy struct {
	Source  string         `json:"source"`
	Columns []CopiedColumn `json:"columns"`
}

func (c Copy) validate() error {
	if !validName(c.Source) || len(c.Columns) == 0 {
		return fmt.Errorf("a YDB row copy names its source and the columns it copies")
	}
	for i, column := range c.Columns {
		if !validName(column.Name) || slices.ContainsFunc(c.Columns[:i], func(other CopiedColumn) bool { return other.Name == column.Name }) {
			return fmt.Errorf("a YDB row copy copies each column once")
		}
		if column.Cast != "" {
			if _, err := schema.StorageType(column.Cast, false); err != nil {
				return fmt.Errorf("a YDB row copy converts to a type YDB stores")
			}
		}
	}
	return nil
}

// Rename is a decoded rename of the owner to Target, in its directory.
type Rename struct {
	Target  string   `json:"target"`
	Columns []string `json:"columns"`
	Key     []string `json:"key"`
}

func (r Rename) validate() error {
	if !validName(r.Target) {
		return fmt.Errorf("a YDB table rename names its target")
	}
	return Drop{Store: RowStore, Columns: r.Columns, Key: r.Key}.validate()
}

// CaptureCopy builds the copy of rows into owner.
func CaptureCopy(id string, owner *protocol.Reference, copy Copy) (*protocol.NormalizedValue, error) {
	return captureStatement(id, owner, CopyModel(), copy, NormalizeCopy)
}

// CaptureRename builds the rename of owner.
func CaptureRename(id string, owner *protocol.Reference, rename Rename) (*protocol.NormalizedValue, error) {
	return captureStatement(id, owner, RenameModel(), rename, NormalizeRename)
}

func captureStatement(id string, owner *protocol.Reference, definition *protocol.ValueModel, payload any,
	normalize func(*protocol.ValueInput) (*protocol.NormalizedValue, error)) (*protocol.NormalizedValue, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return normalize(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

// NormalizeCopy reconstructs a copy's safety facts from its payload.
func NormalizeCopy(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	copy, err := decodeStatement[Copy](input, CopyModel())
	if err != nil {
		return nil, err
	}
	return normalizedStatement(input, CopyModel(), copy, &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE,
		Reason: "Copies the rows of a table into a new one", Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Rows are written into a table the plan created"})
}

// NormalizeRename reconstructs a rename's safety facts from its payload.
func NormalizeRename(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	rename, err := decodeStatement[Rename](input, RenameModel())
	if err != nil {
		return nil, err
	}
	return normalizedStatement(input, RenameModel(), rename, &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "Renames a table", Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "The table keeps its permissions under its new name"})
}

// DecodeCopy returns the copy an envelope holds after validating it.
func DecodeCopy(value *protocol.Envelope) (*Copy, error) {
	return decodeStatement[Copy](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value}, CopyModel())
}

// DecodeRename returns the rename an envelope holds after validating it.
func DecodeRename(value *protocol.Envelope) (*Rename, error) {
	return decodeStatement[Rename](&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value}, RenameModel())
}

type statementPayload interface {
	Copy | Rename
	validate() error
}

func decodeStatement[T statementPayload](input *protocol.ValueInput, definition *protocol.ValueModel) (*T, error) {
	value := input.GetValue()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB table operation does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value.Payload))
	decoder.DisallowUnknownFields()
	var payload T
	if err := decoder.Decode(&payload); err != nil || decoder.More() {
		return nil, fmt.Errorf("invalid YDB table operation payload")
	}
	if err := payload.validate(); err != nil {
		return nil, err
	}
	return &payload, nil
}

func normalizedStatement(input *protocol.ValueInput, definition *protocol.ValueModel, payload any, semantics *protocol.OperationSemantics) (*protocol.NormalizedValue, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	encoded, err = modeljson.Canonical(encoded)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(input.GetValue())
	result.Payload = encoded
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(encoded), OperationSemantics: semantics}, nil
}

// CopyStatement writes the INSERT that copies the rows of a copy into the
// table at path, whose directory holds the source too, as the host's embedded
// engine writes it: each column read as the new table declares it. A changed
// type is converted with CAST, which yields NULL for a value it cannot
// convert, so the value is unwrapped and such a row fails the copy with a
// message naming the table being rebuilt and the column; a NOT NULL column is
// unwrapped too, which YQL requires of an optional value written to one.
func CopyStatement(path string, copy *Copy) (string, error) {
	directory := path[:max(strings.LastIndex(path, "/"), 0)]
	source := TextPath(directory, copy.Source)
	// The embedded engine names the table as the host does, its schema and
	// name joined by a dot.
	label := copy.Source
	if directory != "" {
		label = directory + "." + copy.Source
	}
	names := make([]string, 0, len(copy.Columns))
	values := make([]string, 0, len(copy.Columns))
	for _, column := range copy.Columns {
		value, err := copiedValue(label, column)
		if err != nil {
			return "", err
		}
		names = append(names, QuoteIdentifier(column.Name))
		values = append(values, value+" AS "+QuoteIdentifier(column.Name))
	}
	return "INSERT INTO " + QuoteIdentifier(path) + " (" + strings.Join(names, ", ") + ") SELECT " + strings.Join(values, ", ") +
		" FROM " + QuoteIdentifier(source) + ";", nil
}

func copiedValue(table string, column CopiedColumn) (string, error) {
	quoted := QuoteIdentifier(column.Name)
	unwrapped := func(value, message string) (string, error) {
		literal, err := ydbtype.ValueLiteral("Utf8", message)
		if err != nil {
			return "", err
		}
		return "Unwrap(" + value + ", " + literal + ")", nil
	}
	switch {
	case column.Cast != "" && column.Required:
		return unwrapped("CAST("+quoted+" AS "+column.Cast+")",
			fmt.Sprintf("rebuilding table %s: column %s holds NULL or a value that does not convert to its new type", table, column.Name))
	case column.Cast != "":
		value, err := unwrapped("CAST("+quoted+" AS "+column.Cast+")",
			fmt.Sprintf("rebuilding table %s: column %s holds a value that does not convert to its new type", table, column.Name))
		if err != nil {
			return "", err
		}
		return "IF(" + quoted + " IS NULL, NULL, " + value + ")", nil
	case column.Required:
		return unwrapped(quoted, fmt.Sprintf("rebuilding table %s: column %s holds NULL, and the new table declares it NOT NULL", table, column.Name))
	default:
		return quoted, nil
	}
}

// RenameStatement writes the statement of a rename of the table at path, as
// the host's embedded engine writes it: `ALTER TABLE `old` RENAME TO `new`;`.
func RenameStatement(path string, rename *Rename) string {
	directory := path[:max(strings.LastIndex(path, "/"), 0)]
	return "ALTER TABLE " + QuoteIdentifier(path) + " RENAME TO " + QuoteIdentifier(TextPath(directory, rename.Target)) + ";"
}
