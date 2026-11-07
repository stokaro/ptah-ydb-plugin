package tablecreate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// The properties of a column an omission names, in the host's words.
const (
	// TypeModifierProperty is the part of a column's declared type the YDB
	// type does not keep: the length of a VARCHAR(255), whose Utf8 has none,
	// or a timestamp precision, where YDB keeps microseconds. The column holds
	// every value the declaration admits, so nothing written is lost; the
	// server no longer enforces the limit.
	TypeModifierProperty = "type modifier"
	// IdentityStartProperty is the first value of a Serial's sequence.
	IdentityStartProperty = "identity start"
	// IdentityIncrementProperty is the step of a Serial's sequence.
	IdentityIncrementProperty = "identity increment"
)

// typeModifiersKey is the member of a creation payload that holds its type
// modifiers beside the creation's own fields.
const typeModifiersKey = "typeModifiers"

// Omission is a declared property of a column a creation's statement goes on
// without, as render.omissions reports it: Column names the column, Property
// what it lost and Detail the declared value.
type Omission struct {
	Column   string `json:"column"`
	Property string `json:"property"`
	Detail   string `json:"detail"`
}

// TableName is how an omission names a table, as the host's embedded engine
// named it: its directory and its name joined by a dot.
func TableName(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "." + name
}

// DeclaredOmissions are what an offline render of the creation of table,
// with caps, goes on without, in the order the table declares its columns:
// the part of each column's declared type the YDB type does not keep (see
// [TypeModifierProperty]), and a Serial's start and increment other than 1,
// which only an ALTER SEQUENCE naming the database gives the sequence (see
// [SequenceOmissions]). The host's embedded engine reported both.
func DeclaredOmissions(table *protocol.DeclaredTable, caps map[string]bool) []Omission {
	omissions := typeModifiers(table, caps)
	for _, column := range table.GetColumns() {
		if column.GetIdentityStart() == "" && column.GetIdentityIncrement() == "" {
			continue
		}
		settings, err := ydbsequence.Parse(column.GetIdentityStart(), column.GetIdentityIncrement())
		if err == nil {
			omissions = append(omissions, sequenceOmissions(column.GetName(), settings)...)
		}
	}
	return omissions
}

// typeModifiers are the type modifiers of the columns table declares, in the
// order it declares them. A type YDB has no mapping for is refused elsewhere.
func typeModifiers(table *protocol.DeclaredTable, caps map[string]bool) []Omission {
	var omissions []Omission
	for _, column := range table.GetColumns() {
		mapping, err := ydbtype.Map(column.GetType(), ydbtype.Capabilities(caps))
		if err == nil && mapping.Dropped != "" {
			omissions = append(omissions, Omission{Column: column.GetName(), Property: TypeModifierProperty,
				Detail: column.GetType() + ": " + mapping.Dropped})
		}
	}
	return omissions
}

// SequenceOmissions are the start and the increment of each Serial column of
// request that the text of the creation in the database at database goes
// without: where the text names no database, as an offline render does, no
// ALTER SEQUENCE can name the sequence, and the table is written without
// them, as the host's embedded engine wrote it.
func SequenceOmissions(database string, request *Ydb_Table.CreateTableRequest) []Omission {
	if database != "" {
		return nil
	}
	var omissions []Omission
	for _, column := range request.GetColumns() {
		if sequence := column.GetFromSequence(); sequence != nil {
			omissions = append(omissions, sequenceOmissions(column.GetName(), requestedSettings(sequence))...)
		}
	}
	return omissions
}

// sequenceOmissions are a Serial's start and increment where they are not
// YDB's 1.
func sequenceOmissions(column string, settings ydbsequence.Settings) []Omission {
	var omissions []Omission
	if settings.Start != ydbsequence.Default.Start {
		omissions = append(omissions, Omission{Column: column, Property: IdentityStartProperty, Detail: strconv.FormatInt(settings.Start, 10)})
	}
	if settings.Increment != ydbsequence.Default.Increment {
		omissions = append(omissions, Omission{Column: column, Property: IdentityIncrementProperty, Detail: strconv.FormatInt(settings.Increment, 10)})
	}
	return omissions
}

// SortOmissions orders omissions as the host's embedded engine reported
// them: by column, property and detail.
func SortOmissions(omissions []Omission) {
	slices.SortStableFunc(omissions, func(a, b Omission) int {
		return cmp.Or(cmp.Compare(a.Column, b.Column), cmp.Compare(a.Property, b.Property), cmp.Compare(a.Detail, b.Detail))
	})
}

// CreationOmissions are the type modifiers the creation value carries,
// a row table's or a column table's, and none for any other operation. The
// value must be valid.
func CreationOmissions(value *protocol.Envelope) ([]Omission, error) {
	if value.GetKind() != OperationKind && value.GetKind() != ColumnOperationKind {
		return nil, nil
	}
	_, omissions, err := splitTypeModifiers(value.GetPayload())
	return omissions, err
}

// withTypeModifiers adds omissions, the type modifiers of a creation, to its
// payload, a JSON object, under [typeModifiersKey]. A creation without one
// keeps its payload.
func withTypeModifiers(payload []byte, omissions []Omission) ([]byte, error) {
	if len(omissions) == 0 {
		return payload, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(omissions)
	if err != nil {
		return nil, err
	}
	fields[typeModifiersKey] = encoded
	return json.Marshal(fields)
}

// splitTypeModifiers takes the type modifiers out of a creation payload and
// returns the creation's own fields and the modifiers. Each modifier names a
// column once, as a type modifier, with a detail of one line of text.
func splitTypeModifiers(payload []byte) ([]byte, []Omission, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, nil, err
	}
	raw, found := fields[typeModifiersKey]
	if !found {
		return payload, nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var omissions []Omission
	if err := decoder.Decode(&omissions); err != nil || len(omissions) == 0 {
		return nil, nil, fmt.Errorf("invalid YDB creation type modifiers")
	}
	for i, omission := range omissions {
		if omission.Property != TypeModifierProperty || !validName(omission.Column) || !oneLine(omission.Detail) ||
			slices.ContainsFunc(omissions[:i], func(other Omission) bool { return other.Column == omission.Column }) {
			return nil, nil, fmt.Errorf("invalid YDB creation type modifiers")
		}
	}
	delete(fields, typeModifiersKey)
	rest, err := json.Marshal(fields)
	return rest, omissions, err
}

// checkTypeModifiers refuses type modifiers that name a column the creation
// does not hold.
func checkTypeModifiers(omissions []Omission, columns map[string]bool) error {
	for _, omission := range omissions {
		if !columns[omission.Column] {
			return fmt.Errorf("YDB creation type modifiers name no created column")
		}
	}
	return nil
}

// oneLine reports a non-empty line of UTF-8 text.
func oneLine(text string) bool {
	return strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsAny(text, "\r\n\x00")
}
