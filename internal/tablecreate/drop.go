package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// DropOperationKind identifies the removal of a table and its rows: a row
// table with its indexes and changefeeds, or a column table.
const DropOperationKind = schema.ProviderID + "/table-drop-operation"

// The stores a dropped table is held in.
const (
	RowStore    = "row"
	ColumnStore = "column"
)

var dropModel = buildDropModel()

// DropModel binds the removal payload grammar, its precondition and its
// confirmation.
func DropModel() *protocol.ValueModel { return proto.CloneOf(dropModel) }

func buildDropModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": DropOperationKind, "representation": "operation", "schema_version": 1, "encoding": "json",
		"payload":      "store: row or column; columns: the names of the table's columns in order; key: the table's key columns",
		"execution":    "YQL DROP TABLE through the query service once, outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the path must hold a table of the captured store with exactly the captured columns, in order, and the captured key columns",
		"confirmation": "the path must be gone from its directory",
		"semantics":    "destructive: the table's rows, indexes and changefeeds go with it",
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
	return &protocol.ValueModel{Kind: DropOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: DropOperationKind + "/json-v1"}
}

// Drop is a decoded removal: what the table holds when it is planned, which
// execution checks before it drops the table, so a plan never drops a table
// that took the path since.
type Drop struct {
	Store string `json:"store"`
	// Columns are the names of the table's columns, in order.
	Columns []string `json:"columns"`
	// Key are the table's key columns, in key order where the read reports
	// one.
	Key []string `json:"key"`
}

// validate refuses a removal that does not say what it drops.
func (d Drop) validate() error {
	if d.Store != RowStore && d.Store != ColumnStore {
		return fmt.Errorf("a YDB table removal names a row or a column store")
	}
	if len(d.Columns) == 0 || len(d.Key) == 0 {
		return fmt.Errorf("a YDB table removal states the table's columns and key")
	}
	names := make(map[string]bool, len(d.Columns))
	for _, column := range d.Columns {
		if !validName(column) || names[column] {
			return fmt.Errorf("a YDB table removal states each column once")
		}
		names[column] = true
	}
	for i, key := range d.Key {
		if !names[key] || slices.Contains(d.Key[:i], key) {
			return fmt.Errorf("a YDB table removal's key names its columns once each")
		}
	}
	return nil
}

// CaptureDrop builds the removal of the table owner names, which holds what
// observed reads: its columns, its key and, through its facets, its store.
// The key is in key order where the read of the table's properties states it,
// and in column order otherwise.
func CaptureDrop(id string, owner *protocol.Reference, observed *protocol.ObservedTable) (*protocol.NormalizedValue, error) {
	drop := Drop{Store: RowStore}
	for _, column := range observed.GetColumns() {
		drop.Columns = append(drop.Columns, column.GetName())
		if column.GetPrimaryKey() {
			drop.Key = append(drop.Key, column.GetName())
		}
	}
	for _, record := range observed.GetFeatures().GetFacets().GetRecords() {
		switch record.GetKind() {
		case schema.ColumnStorageProperties:
			drop.Store = ColumnStore
		case schema.TableProperties:
			var held native.DescribeTableResult
			if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err != nil {
				return nil, err
			}
			if len(held.GetPrimaryKey()) != 0 {
				drop.Key = slices.Clone(held.GetPrimaryKey())
			}
		}
	}
	payload, err := json.Marshal(drop)
	if err != nil {
		return nil, err
	}
	definition := DropModel()
	return NormalizeDrop(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: DropOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// NormalizeDrop reconstructs a removal's safety facts from its payload.
func NormalizeDrop(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), DropModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB table removal does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	drop, err := decodeDrop(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(drop)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Drops a table and its rows",
			Access: protocol.ChangeSemantics_NARROWS, AccessReason: "The table's permissions go with it"}}, nil
}

// DecodeDrop returns the removal an envelope holds after validating it.
func DecodeDrop(value *protocol.Envelope) (*Drop, error) {
	normalized, err := NormalizeDrop(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeDrop(normalized.Value.Payload)
}

func decodeDrop(payload []byte) (*Drop, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var drop Drop
	if err := decoder.Decode(&drop); err != nil {
		return nil, fmt.Errorf("invalid YDB table removal payload")
	}
	if decoder.More() {
		return nil, fmt.Errorf("invalid YDB table removal payload")
	}
	if err := drop.validate(); err != nil {
		return nil, err
	}
	return &drop, nil
}

// DropStatement writes the statement of a removal at path, as the host's
// embedded engine writes it: `DROP TABLE `path`;`.
func DropStatement(path string) string {
	return "DROP TABLE " + QuoteIdentifier(path) + ";"
}
