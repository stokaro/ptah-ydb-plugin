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

// AlterOperationKind identifies a native alteration of an existing row table's
// column families.
const AlterOperationKind = schema.ProviderID + "/row-table-alter-operation"

var alterFields = []string{"add_column_families", "alter_column_families", "alter_columns"}
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
		"execution":    "native AlterTable once outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the table must exist as a row table and hold every touched family and every moved column exactly as captured; a family to add must be absent; a family to move into must exist or be added by the same request",
		"confirmation": "read back the whole family set and every column's family; each family and column the request does not name must be unchanged, and the columns and key must be unchanged",
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
	// Request holds the families to add, the families to change, and the columns to move.
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
	encoded, err := encodeAlteration(&Alteration{Request: request, HeldFamilies: change.heldFamilies, HeldColumns: change.heldColumns})
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
	if len(alteration.Request.AlterColumnFamilies)+len(alteration.Request.AlterColumns) > 0 {
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
	if len(request.AddColumnFamilies)+len(request.AlterColumnFamilies)+len(request.AlterColumns) == 0 {
		return fmt.Errorf("YDB table alteration changes nothing")
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
