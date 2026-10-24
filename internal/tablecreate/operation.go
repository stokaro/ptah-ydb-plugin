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

// OperationKind identifies a complete native row-table creation request.
const OperationKind = schema.ProviderID + "/row-table-create-operation"

var nativeFields = []string{"columns", "primary_key", "ttl_settings", "storage_settings", "column_families", "attributes",
	"partitioning_settings", "key_bloom_filter", "read_replicas_settings", "tiering", "store_type", "uniform_partitions", "partition_at_keys"}
var operationModel = buildOperationModel()

// Model binds the native payload grammar, absence precondition, and confirmation.
func Model() *protocol.ValueModel { return proto.CloneOf(operationModel) }

func buildOperationModel() *protocol.ValueModel {
	descriptor, err := protojson.Marshal(protodesc.ToDescriptorProto((&Ydb_Table.CreateTableRequest{}).ProtoReflect().Descriptor()))
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]any{
		"kind": OperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"request": json.RawMessage(descriptor), "allowed_request_fields": nativeFields,
		"column_model": json.RawMessage(schema.ColumnDesiredModel().DefinitionJson),
		"table_model":  json.RawMessage(schema.TableDesiredModel().DefinitionJson),
		"columns":      "distinct names, exact native storage types, explicit matching notNull, optional typed literal; sequences, compression, and encoding refused",
		"keys":         "ordered nonempty distinct existing columns",
		"settings":     "row storage only; no access changes, temporary lifetime, indexes, changefeeds, profiles or compaction policy; a starting layout is either a count of uniform partitions over a Uint32 or Uint64 first key column, or split points of optional tuples over the leading key columns",
		"omission":     "omitted creation settings request server defaults; explicit settings are confirmed after creation",
		"execution":    "native CreateTable once outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "complete exact parent directory listing must establish absence; parent must exist; root ptah_locks is reserved; no atomic compare-and-set",
		"confirmation": "read back columns, ordered primary key, requested settings, and absence of unexpected indexes or changefeeds; a successful create does not establish exclusive ownership",
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
	return &protocol.ValueModel{Kind: OperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: OperationKind + "/json-v1"}
}

// Capture compiles a declaration after its caller established the CREATE lifecycle.
func Capture(id string, owner *protocol.Reference, table *protocol.DeclaredTable) (*protocol.NormalizedValue, error) {
	request, err := Compile(owner, table)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(request)
	if err != nil {
		return nil, err
	}
	definition := Model()
	return Normalize(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: OperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// Normalize reconstructs safety facts from a validated native request.
func Normalize(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), Model()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB table creation does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	request, err := decodeRequest(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(request)
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
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a row table",
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Creation inherits server access policy"}}, nil
}

// Request returns independent native operands after validating the envelope.
func Request(value *protocol.Envelope) (*Ydb_Table.CreateTableRequest, error) {
	normalized, err := Normalize(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeRequest(normalized.Value.Payload)
}

func decodeRequest(payload []byte) (*Ydb_Table.CreateTableRequest, error) {
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return nil, err
	}
	if containsNull(fields) {
		return nil, fmt.Errorf("YDB creation settings cannot be null")
	}
	var request Ydb_Table.CreateTableRequest
	if err := protojson.Unmarshal(canonical, &request); err != nil {
		return nil, fmt.Errorf("invalid YDB table creation JSON")
	}
	if err := only(&request, nativeFields...); err != nil {
		return nil, err
	}
	if request.StoreType != Ydb_Table.StoreType_STORE_TYPE_ROW {
		return nil, fmt.Errorf("YDB creation requires row storage")
	}
	names := make(map[string]bool, len(request.Columns))
	if len(request.Columns) == 0 || len(request.PrimaryKey) == 0 {
		return nil, fmt.Errorf("YDB creation requires columns and an ordered primary key")
	}
	for _, column := range request.Columns {
		if err := only(column, "name", "type", "not_null", "family", "from_literal"); err != nil {
			return nil, err
		}
		if !validName(column.Name) || names[column.Name] {
			return nil, fmt.Errorf("invalid or duplicate YDB creation column")
		}
		names[column.Name] = true
		_, nullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return nil, err
		}
		if column.NotNull == nil || *column.NotNull == nullable {
			return nil, fmt.Errorf("YDB column type and nullability disagree")
		}
		if literal := column.GetFromLiteral(); literal != nil && (!proto.Equal(literal.Type, column.Type) || literal.Value == nil) {
			return nil, fmt.Errorf("invalid YDB typed literal default")
		}
	}
	keys := map[string]bool{}
	for _, key := range request.PrimaryKey {
		if !names[key] || keys[key] {
			return nil, fmt.Errorf("invalid YDB primary key column")
		}
		keys[key] = true
	}
	// Reuse the desired model's validation for native settings rather than
	// accepting a less strict operation payload than the source declaration.
	if err := validateRequestLayout(&request); err != nil {
		return nil, err
	}
	settings, err := protojson.Marshal(&request)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(settings, &raw); err != nil {
		return nil, err
	}
	delete(raw, "columns")
	delete(raw, "storeType")
	// The request states the layout as native typed values, which the check
	// above holds to the rules; the settings model states it as source text.
	delete(raw, "uniformPartitions")
	delete(raw, "partitionAtKeys")
	settings, err = json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	model := schema.TableDesiredModel()
	if _, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties,
		SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "validation"}, Payload: settings}); err != nil {
		return nil, err
	}
	if err := validateReferences(&request, names); err != nil {
		return nil, err
	}
	// Native requests must carry no duplicate family declarations even if their
	// unpopulated settings happen to serialize identically.
	families := []string{}
	for _, family := range request.ColumnFamilies {
		families = append(families, family.Name)
	}
	slices.Sort(families)
	if len(slices.Compact(families)) != len(request.ColumnFamilies) {
		return nil, fmt.Errorf("duplicate YDB column family")
	}
	return &request, nil
}

func containsNull(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case []any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	}
	return false
}
