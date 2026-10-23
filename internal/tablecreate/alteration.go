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
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AlterOperationKind identifies a native alteration of an existing row table:
// its column families and their placements, the addition of nullable columns,
// the removal of columns, its TTL, its automatic partitioning settings, or one
// global secondary index. One operation does exactly one of the six.
const AlterOperationKind = schema.ProviderID + "/row-table-alter-operation"

var alterFields = []string{"add_column_families", "alter_column_families", "alter_columns", "add_columns", "drop_columns", "set_ttl_settings", "drop_ttl_settings", "alter_partitioning_settings", "add_indexes", "drop_indexes"}
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
		"payload":      "request: native AlterTableRequest without session or path; held_families: the touched families as the table held them; held_columns: the family each moved column sat in; held_ttl: the TTL the table held, absent for none; held_partitioning: the whole partitioning the table held; held_indexes: the index a removal drops, as the table held it",
		"families":     "families are added or have only the settings the declaration states changed; a family is never dropped, a setting is never reset, keep_in_memory is never set",
		"columns":      "a column moves between families by name; key columns stay in the default family; a column the plan drops is never named",
		"additions":    "add_columns names columns that are nullable, have no default and sit in the default family; a column that must be placed in another family is moved by a separate family operation after the addition",
		"removals":     "drop_columns names columns that are not part of the key, an index, the TTL or the partitioning",
		"ttl":          "set_ttl_settings states the whole TTL the table holds afterward, a date column with seconds or an integer column with a unit and seconds, and a run interval only to keep or set one; drop_ttl_settings removes it; the column must exist with a type the mode reads",
		"partitioning": "alter_partitioning_settings states only the settings that change, among the automatic partitioning by size and by load, the preferred partition size and the minimum and maximum partition counts; the partition columns are never named, a setting is never reset, and the minimum never exceeds a nonzero maximum",
		"indexes":      "add_indexes names one global sync, async or unique index with its ordered key columns and covered columns and no settings of its own; drop_indexes names one index and held_indexes states it as the table held it; an index is never altered in place",
		"exclusive":    "an operation changes families and placements, or adds columns, or drops columns, or sets or drops the TTL, or changes the partitioning settings, or adds or drops one index, never a mix",
		"execution":    "native AlterTable once outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the table must exist as a row table and hold every touched family and every moved column exactly as captured; a family to add must be absent; a family to move into must exist or be added by the same request; a column to add must be absent; a column to drop must exist and must not be a key column, an index column, the TTL column or a partitioning column; the table must hold exactly the TTL captured, or none; the table must hold exactly the partitioning settings captured; a TTL to set must read an existing column of a type its mode reads; an index to add must be absent and read existing columns; an index to drop must exist exactly as captured",
		"confirmation": "read back the whole family set and every column; each family and column the request does not name must be unchanged, an added column must exist with its type and in the default family, a dropped column must be gone, the TTL must be the one set, or gone for a drop and otherwise unchanged, the partitioning must be the captured settings with the stated ones replaced and otherwise unchanged, an added index must exist, be ready and hold the requested definition, a dropped index must be gone, every other index must be unchanged, and the key must be unchanged",
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
	// HeldTTL is the TTL the table held when planned; nil is none. Only a TTL
	// alteration carries it.
	HeldTTL *Ydb_Table.TtlSettings
	// HeldPartitioning is the whole partitioning the table held when planned.
	// Only a partitioning alteration carries it.
	HeldPartitioning *Ydb_Table.PartitioningSettings
	// HeldIndexes is the index an index removal drops, as the table held it.
	// Only a removal carries it.
	HeldIndexes []*native.TableIndexDescription
}

type heldColumn struct {
	Name   string `json:"name"`
	Family string `json:"family"`
}

type alterPayload struct {
	Request      json.RawMessage   `json:"request"`
	HeldFamilies []json.RawMessage `json:"held_families,omitempty"`
	HeldColumns  []heldColumn      `json:"held_columns,omitempty"`
	HeldTTL      json.RawMessage   `json:"held_ttl,omitempty"`
	// HeldPartitioning is kept as raw JSON so an empty object stays present.
	HeldPartitioning json.RawMessage   `json:"held_partitioning,omitempty"`
	HeldIndexes      []json.RawMessage `json:"held_indexes,omitempty"`
}

// CaptureAlteration builds the operation for a family change planned against a
// table in owner.
func CaptureAlteration(id string, owner *protocol.Reference, change familyChange) (*protocol.NormalizedValue, error) {
	request := &Ydb_Table.AlterTableRequest{AddColumnFamilies: change.add, AlterColumnFamilies: change.alter, AlterColumns: change.move}
	return captureRequest(id, owner, &Alteration{Request: request, HeldFamilies: change.heldFamilies, HeldColumns: change.heldColumns})
}

// CaptureTTLAlteration builds the operation for a TTL change planned against a
// table in owner.
func CaptureTTLAlteration(id string, owner *protocol.Reference, change ttlChange) (*protocol.NormalizedValue, error) {
	request := &Ydb_Table.AlterTableRequest{}
	if change.drop {
		request.TtlAction = &Ydb_Table.AlterTableRequest_DropTtlSettings{DropTtlSettings: &emptypb.Empty{}}
	} else {
		request.TtlAction = &Ydb_Table.AlterTableRequest_SetTtlSettings{SetTtlSettings: change.set}
	}
	return captureRequest(id, owner, &Alteration{Request: request, HeldColumns: map[string]string{}, HeldTTL: change.held})
}

// CapturePartitioningAlteration builds the operation for a change of the
// automatic partitioning settings planned against a table in owner.
func CapturePartitioningAlteration(id string, owner *protocol.Reference, change partitioningChange) (*protocol.NormalizedValue, error) {
	request := &Ydb_Table.AlterTableRequest{AlterPartitioningSettings: change.set}
	return captureRequest(id, owner, &Alteration{Request: request, HeldColumns: map[string]string{}, HeldPartitioning: change.held})
}

// CaptureIndexAlteration builds the operation for the addition or the removal
// of one index planned against a table in owner.
func CaptureIndexAlteration(id string, owner *protocol.Reference, change indexChange) (*protocol.NormalizedValue, error) {
	if change.add == nil {
		request := &Ydb_Table.AlterTableRequest{DropIndexes: []string{change.drop.Name}}
		return captureRequest(id, owner, &Alteration{Request: request, HeldColumns: map[string]string{}, HeldIndexes: []*native.TableIndexDescription{change.drop}})
	}
	index, err := tableIndex(change.add)
	if err != nil {
		return nil, err
	}
	return captureRequest(id, owner, &Alteration{Request: &Ydb_Table.AlterTableRequest{AddIndexes: []*Ydb_Table.TableIndex{index}}, HeldColumns: map[string]string{}})
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
	if alteration.HeldTTL != nil {
		if payload.HeldTTL, err = protojson.Marshal(alteration.HeldTTL); err != nil {
			return nil, err
		}
	}
	if alteration.HeldPartitioning != nil {
		if payload.HeldPartitioning, err = protojson.Marshal(alteration.HeldPartitioning); err != nil {
			return nil, err
		}
	}
	for _, index := range alteration.HeldIndexes {
		encoded, err := protojson.Marshal(index)
		if err != nil {
			return nil, err
		}
		payload.HeldIndexes = append(payload.HeldIndexes, encoded)
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
	case len(alteration.Request.DropIndexes) > 0:
		impact, reason = protocol.ChangeSemantics_DESTRUCTIVE, "Drops a secondary index and the table that implements it"
	case len(alteration.Request.AddIndexes) > 0:
		impact, reason = protocol.ChangeSemantics_ADDITIVE, "Adds a secondary index; YDB builds it in the background"
	case alteration.Request.AlterPartitioningSettings != nil:
		impact, reason = protocol.ChangeSemantics_BEHAVIORAL, "Changes when YDB splits and merges the table's partitions"
	case alteration.Request.TtlAction != nil:
		impact, reason = protocol.ChangeSemantics_BEHAVIORAL, "A TTL decides which rows YDB deletes; restoring a prior TTL cannot recover deleted rows"
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
	if payload.HeldTTL != nil {
		var held Ydb_Table.TtlSettings
		if err := protojson.Unmarshal(payload.HeldTTL, &held); err != nil {
			return nil, fmt.Errorf("invalid held YDB TTL")
		}
		if err := validateTTLSettings(&held); err != nil {
			return nil, err
		}
		alteration.HeldTTL = &held
	}
	if payload.HeldPartitioning != nil {
		var held Ydb_Table.PartitioningSettings
		if err := protojson.Unmarshal(payload.HeldPartitioning, &held); err != nil {
			return nil, fmt.Errorf("invalid held YDB partitioning")
		}
		alteration.HeldPartitioning = &held
	}
	for _, encoded := range payload.HeldIndexes {
		var held native.TableIndexDescription
		if err := protojson.Unmarshal(encoded, &held); err != nil {
			return nil, fmt.Errorf("invalid held YDB index")
		}
		if err := schema.Known(&held); err != nil {
			return nil, err
		}
		alteration.HeldIndexes = append(alteration.HeldIndexes, &held)
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
		len(request.AddColumns) > 0, len(request.DropColumns) > 0, request.TtlAction != nil, request.AlterPartitioningSettings != nil,
		len(request.AddIndexes)+len(request.DropIndexes) > 0} {
		if used {
			groups++
		}
	}
	if groups == 0 {
		return fmt.Errorf("YDB table alteration changes nothing")
	}
	if groups > 1 {
		return fmt.Errorf("YDB table alteration mixes family, column, TTL, partitioning and index changes")
	}
	if err := validateColumnAlteration(request); err != nil {
		return err
	}
	if err := validateTTLAlteration(alteration); err != nil {
		return err
	}
	if err := validatePartitioningAlteration(alteration); err != nil {
		return err
	}
	if err := validateIndexAlteration(alteration); err != nil {
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

// validateTTLAlteration checks a TTL alteration. A set states a whole valid TTL
// that differs from the one held, a removal names a TTL the table held, and a
// request that does not touch the TTL carries no held TTL.
func validateTTLAlteration(alteration *Alteration) error {
	request := alteration.Request
	switch action := request.TtlAction.(type) {
	case nil:
		if alteration.HeldTTL != nil {
			return fmt.Errorf("a held YDB TTL disagrees with the alteration")
		}
	case *Ydb_Table.AlterTableRequest_DropTtlSettings:
		if action.DropTtlSettings == nil || alteration.HeldTTL == nil {
			return fmt.Errorf("YDB TTL removal requires the held TTL")
		}
	case *Ydb_Table.AlterTableRequest_SetTtlSettings:
		if err := validateTTLSettings(action.SetTtlSettings); err != nil {
			return err
		}
		if proto.Equal(action.SetTtlSettings, alteration.HeldTTL) {
			return fmt.Errorf("YDB TTL alteration changes nothing")
		}
	default:
		return fmt.Errorf("unsupported YDB TTL action")
	}
	return nil
}

// validatePartitioningAlteration checks a partitioning alteration. It names
// only the five settings that change after creation, states at least one that
// differs from the held value, keeps a partition size beside enabled
// partitioning by size, and leaves the minimum no higher than a nonzero
// maximum. A request that does not touch the partitioning carries no held
// partitioning.
func validatePartitioningAlteration(alteration *Alteration) error {
	set := alteration.Request.AlterPartitioningSettings
	if set == nil {
		if alteration.HeldPartitioning != nil {
			return fmt.Errorf("a held YDB partitioning disagrees with the alteration")
		}
		return nil
	}
	if alteration.HeldPartitioning == nil {
		return fmt.Errorf("YDB partitioning alteration requires the held partitioning")
	}
	if err := only(set, "partitioning_by_size", "partitioning_by_load", "partition_size_mb", "min_partitions_count", "max_partitions_count"); err != nil {
		return err
	}
	if partitioningDelta(set, alteration.HeldPartitioning) == nil {
		return fmt.Errorf("YDB partitioning alteration changes nothing")
	}
	if reason := partitioningRefusal(alteration.HeldPartitioning, set); reason != "" {
		return fmt.Errorf("YDB partitioning alteration is invalid: %s", reason)
	}
	return nil
}

// validateIndexAlteration checks an index alteration. It adds or drops exactly
// one index. An addition names a global sync, async or unique index with at
// least one key column, no repeated column and no settings of its own; a
// removal states the index the table held. A request that does not touch the
// indexes carries no held index.
func validateIndexAlteration(alteration *Alteration) error {
	request := alteration.Request
	if len(request.AddIndexes)+len(request.DropIndexes) == 0 {
		if len(alteration.HeldIndexes) != 0 {
			return fmt.Errorf("a held YDB index disagrees with the alteration")
		}
		return nil
	}
	if len(request.AddIndexes)+len(request.DropIndexes) != 1 {
		return fmt.Errorf("YDB index alteration changes exactly one index")
	}
	if len(request.DropIndexes) == 1 {
		if len(alteration.HeldIndexes) != 1 || alteration.HeldIndexes[0].Name != request.DropIndexes[0] || !validName(request.DropIndexes[0]) {
			return fmt.Errorf("YDB index removal requires the held index")
		}
		return nil
	}
	if len(alteration.HeldIndexes) != 0 {
		return fmt.Errorf("a held YDB index disagrees with the alteration")
	}
	index := request.AddIndexes[0]
	if err := only(index, "name", "index_columns", "data_columns", "global_index", "global_async_index", "global_unique_index"); err != nil {
		return err
	}
	if !validName(index.Name) || len(index.IndexColumns) == 0 {
		return fmt.Errorf("invalid YDB index addition")
	}
	columns := map[string]bool{}
	for _, column := range slices.Concat(index.IndexColumns, index.DataColumns) {
		if !validName(column) || columns[column] {
			return fmt.Errorf("invalid or duplicate YDB index column")
		}
		columns[column] = true
	}
	var settings proto.Message
	switch kind := index.Type.(type) {
	case *Ydb_Table.TableIndex_GlobalIndex:
		settings = kind.GlobalIndex
	case *Ydb_Table.TableIndex_GlobalAsyncIndex:
		settings = kind.GlobalAsyncIndex
	case *Ydb_Table.TableIndex_GlobalUniqueIndex:
		settings = kind.GlobalUniqueIndex
	default:
		return fmt.Errorf("YDB adds only global sync, async and unique indexes to an existing table")
	}
	if settings == nil || proto.Size(settings) != 0 {
		return fmt.Errorf("YDB index addition cannot state index settings")
	}
	return nil
}

// validateTTLSettings requires one mode with a valid column and, for an integer
// column, a unit the server reads. The server also refuses a run interval below
// its limit; the limit is configuration, so it is left to the server.
func validateTTLSettings(ttl *Ydb_Table.TtlSettings) error {
	switch mode := ttl.GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		if !validName(mode.DateTypeColumn.GetColumnName()) {
			return fmt.Errorf("invalid YDB TTL column")
		}
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		unit := mode.ValueSinceUnixEpoch.GetColumnUnit()
		// A unit the enum does not name is refused earlier, with every unknown value.
		if !validName(mode.ValueSinceUnixEpoch.GetColumnName()) || unit == Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_UNSPECIFIED {
			return fmt.Errorf("invalid YDB epoch TTL column or unit")
		}
	default:
		return fmt.Errorf("YDB TTL requires one explicit mode")
	}
	return nil
}
