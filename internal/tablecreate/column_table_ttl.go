package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ColumnTTLOperationKind identifies one step of an in-place change of a column
// table's TTL.
const ColumnTTLOperationKind = schema.ProviderID + "/column-table-ttl-operation"

var columnTTLOperationModel = buildColumnTTLOperationModel()

// ColumnTTLModel binds the payload of a column table's TTL step, its
// precondition and its confirmation.
func ColumnTTLModel() *protocol.ValueModel { return proto.CloneOf(columnTTLOperationModel) }

func buildColumnTTLOperationModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": ColumnTTLOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"message":      string((&native.ColumnTableTTLAlteration{}).ProtoReflect().Descriptor().FullName()),
		"ttl":          "held and set are each absent for none, or tiers that grow strictly, of which only the last may delete and every other moves rows to an external data source named by its absolute path; they differ, and at least one is present",
		"render":       "YQL ALTER TABLE ... RESET (TTL) without set, ALTER TABLE ... SET (TTL = ...) with it, as the host's embedded engine writes them",
		"execution":    "the statement once through the query service, outside SQL transactions; no retries; the absolute path is bound at dispatch",
		"precondition": "the table is a column table and holds exactly the held TTL; a TTL to set reads a column the table holds, first in its key, of a type its mode reads; the session must name the monitoring endpoint, which the read uses",
		"confirmation": "read the table service's and the monitoring endpoint's descriptions back: the table holds the set TTL, or none after a reset",
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
	return &protocol.ValueModel{Kind: ColumnTTLOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: ColumnTTLOperationKind + "/json-v1"}
}

// CaptureColumnTTLAlteration builds the operation for one TTL step planned
// against a column table in owner.
func CaptureColumnTTLAlteration(id string, owner *protocol.Reference, alteration *native.ColumnTableTTLAlteration) (*protocol.NormalizedValue, error) {
	payload, err := protojson.Marshal(alteration)
	if err != nil {
		return nil, err
	}
	definition := ColumnTTLModel()
	return NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: ColumnTTLOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// NormalizeColumnTTLAlteration validates a column table's TTL step and
// reconstructs its safety facts from the payload, never from the sender.
func NormalizeColumnTTLAlteration(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), ColumnTTLModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB column table TTL step does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	alteration, err := decodeColumnTTLAlteration(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(alteration)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	reason := "Sets the TTL of a column table, which decides which rows YDB moves and deletes; restoring a prior TTL cannot recover deleted rows"
	if alteration.Set == nil {
		reason = "Removes the TTL of a column table, so YDB stops moving and deleting its rows"
	}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: reason,
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Alteration inherits server access policy"}}, nil
}

// ColumnTTLAlteration returns the TTL step an envelope carries after validating
// it.
func ColumnTTLAlteration(value *protocol.Envelope) (*native.ColumnTableTTLAlteration, error) {
	normalized, err := NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeColumnTTLAlteration(normalized.Value.Payload)
}

func decodeColumnTTLAlteration(payload []byte) (*native.ColumnTableTTLAlteration, error) {
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return nil, err
	}
	if containsNull(fields) {
		return nil, fmt.Errorf("YDB column table TTL step settings cannot be null")
	}
	var alteration native.ColumnTableTTLAlteration
	if err := protojson.Unmarshal(canonical, &alteration); err != nil {
		return nil, fmt.Errorf("invalid YDB column table TTL step JSON")
	}
	if err := schema.Known(&alteration); err != nil {
		return nil, err
	}
	for _, ttl := range []*native.ColumnTTLPolicy{alteration.Held, alteration.Set} {
		if err := validateColumnTTL(ttl); err != nil {
			return nil, err
		}
	}
	if ColumnTTLEqual(alteration.Held, alteration.Set) {
		return nil, fmt.Errorf("a column table TTL step changes the TTL")
	}
	return &alteration, nil
}

// validateColumnTTL holds a TTL of a step to the grammar of a declared one: absent,
// or enabled with tiers a column table takes.
func validateColumnTTL(ttl *native.ColumnTTLPolicy) error {
	if ttl == nil {
		return nil
	}
	if ttl.GetEnabled() == nil {
		return fmt.Errorf("a column table TTL step states a TTL or none")
	}
	payload, err := protojson.Marshal(&native.ColumnStorageIntent{Ttl: ttl})
	if err != nil {
		return err
	}
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	_, err = schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "validation"}, Payload: payload})
	return err
}

// ColumnTTLStatement writes the statement of a valid TTL step at an absolute
// path, as the host's embedded engine writes it: `ALTER TABLE `path` RESET
// (TTL);` or `ALTER TABLE `path` SET (TTL = ...);`.
func ColumnTTLStatement(path string, alteration *native.ColumnTableTTLAlteration) string {
	prefix := "ALTER TABLE " + QuoteIdentifier(path) + " "
	if alteration.Set == nil {
		return prefix + "RESET (TTL);"
	}
	return prefix + "SET (TTL = " + TTLClause(alteration.Set) + ");"
}
