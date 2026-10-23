package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
)

// AlterOperationKind identifies a native alteration of an existing row table:
// its column families and their placements, the addition of nullable columns,
// or the removal of columns. One operation does exactly one of the three.
const AlterOperationKind = schema.ProviderID + "/row-table-alter-operation"

var alterFields = []string{"add_column_families", "alter_column_families", "alter_columns", "add_columns", "drop_columns"}
var alterModel = buildAlterModel()

// AlterModel binds the alteration payload grammar, its preconditions, and its
// confirmation.
func AlterModel() *protocol.ValueModel { return proto.CloneOf(alterModel) }

func buildAlterModel() *protocol.ValueModel {
	descriptor, err := protojson.Marshal(protodesc.ToDescriptorProto((&Ydb_Table.AlterTableRequest{}).ProtoReflect().Descriptor()))
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]any{
		"kind": AlterOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"request": json.RawMessage(descriptor), "allowed_request_fields": alterFields,
		"payload":      "request: native AlterTableRequest without session or path; held_families: the touched families as the table held them; held_columns: the family each moved column sat in",
		"families":     "families are added or have only the settings the declaration states changed; a family is never dropped, a setting is never reset, keep_in_memory is never set",
		"columns":      "a column moves between families by name; key columns stay in the default family; a column the plan drops is never named",
		"additions":    "add_columns names columns that are nullable, have no default and sit in the default family; a column that must be placed in another family is moved by a separate family operation after the addition",
		"removals":     "drop_columns names columns that are not part of the key, an index, the TTL or the partitioning",
		"exclusive":    "an operation changes families and placements, or adds columns, or drops columns, never a mix",
		"execution":    "native AlterTable once outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the table must exist as a row table and hold every touched family and every moved column exactly as captured; a family to add must be absent; a family to move into must exist or be added by the same request; a column to add must be absent; a column to drop must exist and must not be a key column, an index column, the TTL column or a partitioning column",
		"confirmation": "read back the whole family set and every column; each family and column the request does not name must be unchanged, an added column must exist with its type and in the default family, a dropped column must be gone, and the key must be unchanged",
		"database":     "an explicit owner database must match the selected session; an empty database binds to that session",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: AlterOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: AlterOperationKind + "/json-v1"}
}

// Alteration is a decoded column family alteration. The request carries no
// session and no path: execution binds both after checking the captured owner.
type Alteration struct {
	// Request holds the families to add, the families to change, the columns to
	// move, the columns to add, or the columns to drop.
	Request *Ydb_Table.AlterTableRequest
	// HeldFamilies are the families Request changes, as the table held them when planned.
	HeldFamilies []*Ydb_Table.ColumnFamily
	// HeldColumns maps each moved column to the family it sat in; empty is the default family.
	HeldColumns map[string]string
}

type heldColumn struct {
	Name   string `json:"name"`
	Family string `json:"family"`
}

type alterPayload struct {
	Request      json.RawMessage   `json:"request"`
	HeldFamilies []json.RawMessage `json:"held_families,omitempty"`
	HeldColumns  []heldColumn      `json:"held_columns,omitempty"`
}

// CaptureAlteration builds the operation for a family change planned against a
// table in owner.
func CaptureAlteration(id string, owner *protocol.Reference, change familyChange) (*protocol.NormalizedValue, error) {
	request := &Ydb_Table.AlterTableRequest{AddColumnFamilies: change.add, AlterColumnFamilies: change.alter, AlterColumns: change.move}
	return captureRequest(id, owner, &Alteration{Request: request, HeldFamilies: change.heldFamilies, HeldColumns: change.heldColumns})
}

func captureRequest(id string, owner *protocol.Reference, alteration *Alteration) (*protocol.NormalizedValue, error) {
	encoded, err := encodeAlteration(alteration)
	if err != nil {
		return nil, err
	}
	definition := AlterModel()
	return NormalizeAlteration(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: AlterOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

func encodeAlteration(alteration *Alteration) ([]byte, error) {
	request, err := protojson.Marshal(alteration.Request)
	if err != nil {
		return nil, err
	}
	payload := alterPayload{Request: request}
	for _, family := range alteration.HeldFamilies {
		encoded, err := protojson.Marshal(family)
		if err != nil {
			return nil, err
		}
		payload.HeldFamilies = append(payload.HeldFamilies, encoded)
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range alteration.HeldColumns {
			if !yield(name) {
				return
			}
		}
	}) {
		payload.HeldColumns = append(payload.HeldColumns, heldColumn{Name: name, Family: alteration.HeldColumns[name]})
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return modeljson.Canonical(raw)
}

// NormalizeAlteration validates a native alteration and reconstructs its safety
// facts from the request, never from the sender.
func NormalizeAlteration(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), AlterModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB table alteration does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	alteration, err := decodeAlteration(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := encodeAlteration(alteration)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	impact, reason := protocol.ChangeSemantics_ADDITIVE, "Adds column families"
	switch {
	case len(alteration.Request.DropColumns) > 0:
		impact, reason = protocol.ChangeSemantics_DESTRUCTIVE, "Drops columns and the data they hold"
	case len(alteration.Request.AddColumns) > 0:
		impact, reason = protocol.ChangeSemantics_ADDITIVE, "Adds nullable columns"
	case len(alteration.Request.AlterColumnFamilies)+len(alteration.Request.AlterColumns) > 0:
		impact, reason = protocol.ChangeSemantics_BEHAVIORAL, "Changes column family settings or moves columns between families"
	}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: impact, Reason: reason,
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Alteration inherits server access policy"}}, nil
}

// DecodeAlteration returns independent native operands after validating the envelope.
func DecodeAlteration(value *protocol.Envelope) (*Alteration, error) {
	normalized, err := NormalizeAlteration(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeAlteration(normalized.Value.Payload)
}

func decodeAlteration(raw []byte) (*Alteration, error) {
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return nil, err
	}
	var payload alterPayload
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid YDB table alteration JSON")
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil || containsNull(fields) {
		return nil, fmt.Errorf("YDB alteration settings cannot be null")
	}
	alteration := &Alteration{Request: &Ydb_Table.AlterTableRequest{}, HeldColumns: map[string]string{}}
	if err := protojson.Unmarshal(payload.Request, alteration.Request); err != nil {
		return nil, fmt.Errorf("invalid YDB table alteration request")
	}
	if err := only(alteration.Request, alterFields...); err != nil {
		return nil, err
	}
	for _, encoded := range payload.HeldFamilies {
		var family Ydb_Table.ColumnFamily
		if err := protojson.Unmarshal(encoded, &family); err != nil {
			return nil, fmt.Errorf("invalid held YDB column family")
		}
		if err := only(&family, "name", "data", "compression", "keep_in_memory"); err != nil {
			return nil, err
		}
		alteration.HeldFamilies = append(alteration.HeldFamilies, &family)
	}
	for _, held := range payload.HeldColumns {
		if _, duplicate := alteration.HeldColumns[held.Name]; duplicate {
			return nil, fmt.Errorf("duplicate held YDB column")
		}
		alteration.HeldColumns[held.Name] = held.Family
	}
	return alteration, validateAlteration(alteration)
}

func validateAlteration(alteration *Alteration) error {
	request := alteration.Request
	groups := 0
	for _, used := range []bool{len(request.AddColumnFamilies)+len(request.AlterColumnFamilies)+len(request.AlterColumns) > 0,
		len(request.AddColumns) > 0, len(request.DropColumns) > 0} {
		if used {
			groups++
		}
	}
	if groups == 0 {
		return fmt.Errorf("YDB table alteration changes nothing")
	}
	if groups > 1 {
		return fmt.Errorf("YDB table alteration mixes family, column addition and column removal changes")
	}
	if err := validateColumnAlteration(request); err != nil {
		return err
	}
	named := map[string]bool{}
	for _, family := range slices.Concat(request.AddColumnFamilies, request.AlterColumnFamilies) {
		if err := only(family, "name", "data", "compression"); err != nil {
			return err
		}
		if family.Data != nil {
			if err := only(family.Data, "media"); err != nil {
				return err
			}
		}
		if !validName(family.Name) || named[family.Name] {
			return fmt.Errorf("invalid or duplicate YDB column family in an alteration")
		}
		named[family.Name] = true
	}
	for _, family := range request.AlterColumnFamilies {
		if family.Data.GetMedia() == "" && family.Compression == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
			return fmt.Errorf("YDB family alteration states no setting")
		}
	}
	held := map[string]bool{}
	for _, family := range alteration.HeldFamilies {
		held[family.Name] = true
	}
	if len(held) != len(alteration.HeldFamilies) || len(held) != len(request.AlterColumnFamilies) {
		return fmt.Errorf("held YDB column families disagree with the alteration")
	}
	for _, family := range request.AlterColumnFamilies {
		if !held[family.Name] {
			return fmt.Errorf("held YDB column families disagree with the alteration")
		}
	}
	columns := map[string]bool{}
	for _, column := range request.AlterColumns {
		if err := only(column, "name", "family"); err != nil {
			return err
		}
		if !validName(column.Name) || columns[column.Name] || !validName(column.Family) {
			return fmt.Errorf("invalid or duplicate YDB column move")
		}
		columns[column.Name] = true
		if _, found := alteration.HeldColumns[column.Name]; !found {
			return fmt.Errorf("held YDB columns disagree with the alteration")
		}
	}
	if len(alteration.HeldColumns) != len(columns) {
		return fmt.Errorf("held YDB columns disagree with the alteration")
	}
	return nil
}

// validateColumnAlteration checks the columns an alteration adds or drops. An
// added column is nullable, has no default and states no family: the server
// refuses a NOT NULL addition, and a default is accepted on some lines only.
func validateColumnAlteration(request *Ydb_Table.AlterTableRequest) error {
	names := map[string]bool{}
	for _, column := range request.AddColumns {
		if err := only(column, "name", "type", "not_null"); err != nil {
			return err
		}
		if !validName(column.Name) || names[column.Name] {
			return fmt.Errorf("invalid or duplicate YDB column addition")
		}
		names[column.Name] = true
		_, nullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return err
		}
		if !nullable || column.NotNull == nil || *column.NotNull {
			return fmt.Errorf("YDB adds only nullable columns to an existing table")
		}
	}
	for _, name := range request.DropColumns {
		if !validName(name) || names[name] {
			return fmt.Errorf("invalid or duplicate YDB column removal")
		}
		names[name] = true
	}
	return nil
}
