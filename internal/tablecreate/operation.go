package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"ptah.run/provider-sdk/modeljson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// OperationKind identifies a complete native row-table creation request.
const OperationKind = schema.ProviderID + "/row-table-create-operation"

var nativeFields = []string{"columns", "primary_key", "ttl_settings", "storage_settings", "column_families", "attributes",
	"partitioning_settings", "key_bloom_filter", "read_replicas_settings", "tiering", "store_type", "uniform_partitions", "partition_at_keys", "indexes"}
var operationModel = buildOperationModel()

// Model binds the native payload grammar, absence precondition, and confirmation.
func Model() *protocol.ValueModel { return proto.CloneOf(operationModel) }

func buildOperationModel() *protocol.ValueModel {
	descriptor, err := protojson.Marshal(protodesc.ToDescriptorProto((&Ydb_Table.CreateTableRequest{}).ProtoReflect().Descriptor()))
	if err != nil {
		panic(err)
	}
	family, err := protojson.Marshal(protodesc.ToDescriptorProto((&native.ColumnFamily{}).ProtoReflect().Descriptor()))
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]any{
		"kind": OperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"request": json.RawMessage(descriptor), "allowed_request_fields": nativeFields, "column_family": json.RawMessage(family),
		"column_model": json.RawMessage(schema.ColumnDesiredModel().DefinitionJson),
		"table_model":  json.RawMessage(schema.TableDesiredModel().DefinitionJson),
		"columns":      "distinct names, exact native storage types, explicit matching notNull, optional typed literal or Serial sequence; compression and encoding refused",
		"sequences": "a Serial column is a NOT NULL Int16, Int32 or Int64 whose sequence is named _serial_column_<column> and states min 1, the " +
			"maximum of the column's type, a start from 1 to that maximum, cache 1, an increment of at least 1 and no cycle; confirmation reads the same " +
			"sequence back with no restart",
		"keys": "ordered nonempty distinct existing columns",
		"families": "each column family is written as column_family describes it, Ydb.Table.ColumnFamily with the cache mode at its upstream field " +
			"6, which the request carries to the table service; a cache mode requires the column_family_cache_mode capability",
		"settings": "row storage only; no access changes, temporary lifetime, changefeeds, profiles or compaction policy; a starting layout is " +
			"either a count of uniform partitions over a Uint32 or Uint64 first key column, or split points of optional tuples over the leading key columns",
		"indexes": "global indexes, sync, async or unique, each with its ordered key columns and covered columns over declared columns and no settings of its own, " +
			"and vector and full-text indexes, each with its definition and no settings of the tables that implement it, carried as the upstream arm " +
			"(fields 7, 8 and 9 of Ydb.Table.TableIndex) the pinned bindings keep as an unknown field; all in name order, built with the table",
		"omission": "omitted creation settings request server defaults; explicit settings are confirmed after creation",
		"type_modifiers": "an optional typeModifiers member beside the request's fields: each created column whose declared type YDB keeps only in part, " +
			"once, with property \"type modifier\" and the declared type and what YDB does not keep of it as its detail; a render reports each as an " +
			"omission, and execution ignores them",
		"plan_database": "an optional database member beside the request's fields: the clean absolute path of the database the plan read, carried by " +
			"a creation whose Serial sequence gets a start or an increment, so the TEXT form names the sequence's absolute path in its ALTER SEQUENCE; " +
			"execution ignores it",
		"execution": "native CreateTable once outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the complete listing of the parent directory must establish absence; a parent directory YDB does not show establishes absence " +
			"when the listing of its own parent, proved the same way up to the database root, does not name it, and YDB creates it with the table; root " +
			"ptah_locks is reserved; no atomic compare-and-set",
		"confirmation": "read back columns, ordered primary key, requested settings, the requested indexes ready with their definitions, a vector or full-text index's " +
			"settings among them, and no other index, and no changefeed; a successful create does not establish exclusive ownership",
		"database": "an explicit owner database must match the selected session; an empty database binds to that session",
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

// Capture compiles a declaration after its caller established the CREATE
// lifecycle. The creation carries the part of each column's declared type the
// YDB type does not keep, which a render reports (see [CreationOmissions]).
func Capture(id string, owner *protocol.Reference, table *protocol.DeclaredTable, caps map[string]bool) (*protocol.NormalizedValue, error) {
	request, err := Compile(owner, table, caps)
	if err != nil {
		return nil, err
	}
	return captureCreation(id, owner, request, typeModifiers(table, caps))
}

// CaptureRequest builds the creation of a compiled native request at owner,
// which a rebuild names with the scratch table's name.
func CaptureRequest(id string, owner *protocol.Reference, request *Ydb_Table.CreateTableRequest) (*protocol.NormalizedValue, error) {
	return captureCreation(id, owner, request, nil)
}

// captureCreation is [CaptureRequest] for a creation that carries the type
// modifiers omissions.
func captureCreation(id string, owner *protocol.Reference, request *Ydb_Table.CreateTableRequest, omissions []Omission) (*protocol.NormalizedValue, error) {
	payload, err := marshalRequest(request)
	if err != nil {
		return nil, err
	}
	if payload, err = withTypeModifiers(payload, omissions); err != nil {
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
	fields, database, err := splitDatabase(value.Payload)
	if err != nil {
		return nil, err
	}
	fields, omissions, err := splitTypeModifiers(fields)
	if err != nil {
		return nil, err
	}
	request, err := decodeRequest(fields)
	if err != nil {
		return nil, err
	}
	if err := checkTypeModifiers(omissions, columnSet(request.GetColumns())); err != nil {
		return nil, err
	}
	payload, err := marshalRequest(request)
	if err != nil {
		return nil, err
	}
	if payload, err = withTypeModifiers(payload, omissions); err != nil {
		return nil, err
	}
	if payload, err = withDatabase(payload, database); err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	access, accessReason := createdAccess(result.GetOwner().GetName())
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a row table",
			Access: access, AccessReason: accessReason}}, nil
}

// Request returns independent native operands after validating the envelope.
func Request(value *protocol.Envelope) (*Ydb_Table.CreateTableRequest, error) {
	normalized, err := Normalize(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	fields, _, err := splitDatabase(normalized.Value.Payload)
	if err != nil {
		return nil, err
	}
	fields, _, err = splitTypeModifiers(fields)
	if err != nil {
		return nil, err
	}
	return decodeRequest(fields)
}

// CreationDatabase is the database a valid row-table creation names as the
// one its plan read, or "" for one that names none.
func CreationDatabase(value *protocol.Envelope) (string, error) {
	if value.GetKind() != OperationKind {
		return "", nil
	}
	_, database, err := splitDatabase(value.GetPayload())
	return database, err
}

// CreationInDatabase is the row-table creation operation with the database at
// database, the path of the database the plan read, when its request gives a
// Serial's sequence a start or an increment, and the operation unchanged
// otherwise. The host names a table it plans by no database, so the owner
// names none, and the TEXT form, whose ALTER SEQUENCE names the sequence by its
// absolute path, takes the database from here. Without it a migration file
// creates the sequence with YDB's start and increment of 1 (see
// createdSequenceStatements).
func CreationInDatabase(operation *protocol.NormalizedValue, database string) (*protocol.NormalizedValue, error) {
	value := operation.GetValue()
	if database == "" || value.GetKind() != OperationKind || value.GetOwner().GetDatabase() != "" {
		return operation, nil
	}
	request, err := Request(value)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(request.GetColumns(), func(column *Ydb_Table.ColumnMeta) bool {
		return column.GetFromSequence() != nil && !requestedSettings(column.GetFromSequence()).IsDefault()
	}) {
		return operation, nil
	}
	payload, err := withDatabase(value.GetPayload(), database)
	if err != nil {
		return nil, err
	}
	envelope := proto.CloneOf(value)
	envelope.Payload = payload
	return Normalize(&protocol.ValueInput{Id: operation.GetId(), Representation: protocol.ValueInput_OPERATION, Value: envelope})
}

// withDatabase adds database, a clean absolute path, to a creation payload
// under [databaseKey]; an empty one adds nothing.
func withDatabase(payload []byte, database string) ([]byte, error) {
	if database == "" {
		return payload, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(database)
	if err != nil {
		return nil, err
	}
	fields[databaseKey] = encoded
	return json.Marshal(fields)
}

// splitDatabase takes the database out of a creation payload and returns the
// rest and the database, "" for a payload that names none. A database is a
// clean absolute path.
func splitDatabase(payload []byte) ([]byte, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, "", err
	}
	raw, found := fields[databaseKey]
	if !found {
		return payload, "", nil
	}
	var database string
	if err := json.Unmarshal(raw, &database); err != nil || !path.IsAbs(database) || path.Clean(database) != database {
		return nil, "", fmt.Errorf("a YDB table creation names its database by a clean absolute path")
	}
	delete(fields, databaseKey)
	rest, err := json.Marshal(fields)
	return rest, database, err
}

// columnSet is the set of the names of columns.
func columnSet[C interface{ GetName() string }](columns []C) map[string]bool {
	names := make(map[string]bool, len(columns))
	for _, column := range columns {
		names[column.GetName()] = true
	}
	return names
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
	if err := unmarshalRequest(canonical, &request); err != nil {
		return nil, fmt.Errorf("invalid YDB table creation JSON")
	}
	if err := only(&request, nativeFields...); err != nil {
		return nil, err
	}
	if request.StoreType != Ydb_Table.StoreType_STORE_TYPE_ROW {
		return nil, fmt.Errorf("YDB creation requires row storage")
	}
	names, err := requestColumnNames(&request)
	if err != nil {
		return nil, err
	}
	if err := validateIndexes(&request, names); err != nil {
		return nil, err
	}
	// Reuse the desired model's validation for native settings rather than
	// accepting a less strict operation payload than the source declaration.
	if err := validateRequestLayout(&request); err != nil {
		return nil, err
	}
	if err := validateRequestSettings(&request); err != nil {
		return nil, err
	}
	if err := validateReferences(&request, names); err != nil {
		return nil, err
	}
	// Native requests must carry no duplicate family declarations even if their
	// unpopulated settings happen to serialize identically.
	families := make([]string, 0)
	for _, family := range request.ColumnFamilies {
		families = append(families, family.Name)
	}
	slices.Sort(families)
	if len(slices.Compact(families)) != len(request.ColumnFamilies) {
		return nil, fmt.Errorf("duplicate YDB column family")
	}
	return &request, nil
}

// requestColumnNames checks the columns and the key of a creation request and
// returns the set of column names. Each column has a valid distinct name, a
// type with its matching nullability and at most a typed literal default or a
// Serial sequence; the key names distinct columns.
func requestColumnNames(request *Ydb_Table.CreateTableRequest) (map[string]bool, error) {
	names := make(map[string]bool, len(request.Columns))
	if len(request.Columns) == 0 || len(request.PrimaryKey) == 0 {
		return nil, fmt.Errorf("YDB creation requires columns and an ordered primary key")
	}
	for _, column := range request.Columns {
		if err := only(column, "name", "type", "not_null", "family", "from_literal", "from_sequence"); err != nil {
			return nil, err
		}
		if err := validateSerial(column); err != nil {
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
	keys := make(map[string]bool)
	for _, key := range request.PrimaryKey {
		if !names[key] || keys[key] {
			return nil, fmt.Errorf("invalid YDB primary key column")
		}
		keys[key] = true
	}
	return names, nil
}

// validateRequestSettings holds the table settings of a creation request to
// the desired table model: the request without its columns, its storage
// kind, its indexes and its layout.
func validateRequestSettings(request *Ydb_Table.CreateTableRequest) error {
	settings, err := marshalRequest(request)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(settings, &raw); err != nil {
		return err
	}
	delete(raw, "columns")
	delete(raw, "storeType")
	delete(raw, "indexes")
	// The request states the layout as native typed values, which
	// validateRequestLayout holds to the rules; the settings model states it
	// as source text.
	delete(raw, "uniformPartitions")
	delete(raw, "partitionAtKeys")
	settings, err = json.Marshal(raw)
	if err != nil {
		return err
	}
	model := schema.TableDesiredModel()
	_, err = schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties,
		SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "validation"}, Payload: settings})
	return err
}

func containsNull(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case []any:
		if slices.ContainsFunc(v, containsNull) {
			return true
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
