package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ColumnOperationKind identifies the creation of a column table.
const ColumnOperationKind = schema.ProviderID + "/column-table-create-operation"

// TieredTTLCapability is the session capability a column table's TTL tier that
// moves rows to an external data source requires: YDB refuses one while
// EnableTieringInColumnShard is off (`Tiering functionality is disabled for
// OLAP tables`).
const TieredTTLCapability = "tiered_ttl"

var columnOperationModel = buildColumnOperationModel()

// ColumnModel binds the column-table creation payload, its precondition and its
// confirmation.
func ColumnModel() *protocol.ValueModel { return proto.CloneOf(columnOperationModel) }

func buildColumnOperationModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": ColumnOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"message":      string((&native.ColumnTableCreation{}).ProtoReflect().Descriptor().FullName()),
		"columns":      "distinct names, exact native storage types and an explicit matching notNull; no family, default, compression or encoding; key columns are not null",
		"keys":         "ordered nonempty distinct existing columns; hash columns, when stated, are distinct key columns; without them YDB hashes by the whole key",
		"shards":       "a stated count of at least 1 that fits 32 bits; without one YDB chooses",
		"ttl":          "absent for none, or tiers that grow strictly, of which only the last may delete and every other moves rows to an external data source named by its absolute path; the column is declared, first in the key, and of a type the mode reads: a date type without a unit, Uint32, Uint64 or DyNumber with one",
		"render":       "YQL CREATE TABLE ... PARTITION BY HASH (...) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = n, TTL = ...), as the host's embedded engine writes it; the native CreateTable call writes another hash function (HASH_FUNCTION_MODULO_N), which the host does not read",
		"execution":    "the statement once through the query service, outside SQL transactions; no retries; the absolute path is bound at dispatch",
		"precondition": "the complete exact parent directory listing must establish absence; the parent must exist; root ptah_locks is reserved; the session must name the monitoring endpoint, which the confirmation reads",
		"confirmation": "read the table service's and the monitoring endpoint's descriptions back: the columns in order with their types and nullability, the key, the hash key the request states or the whole key, the shard count the request states, the TTL and no local index",
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
	return &protocol.ValueModel{Kind: ColumnOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: ColumnOperationKind + "/json-v1"}
}

// IsColumnTable reports whether a captured declaration declares a column table:
// it carries the column-storage facet for this target.
func IsColumnTable(table *protocol.DeclaredTable) bool {
	return slices.ContainsFunc(table.GetFeatures().GetFacets().GetRecords(), func(record *protocol.ComparisonRecord) bool {
		return record.GetKind() == schema.ColumnStorageProperties && record.GetValue() != nil && forThisEngine(record.GetTargets())
	})
}

// CompileColumnTable compiles a column table's declaration into its creation.
// The result has no path: execution binds it after checking the captured
// owner. A declaration CREATE TABLE cannot carry, or that YDB refuses only
// after the statement runs, is refused.
func CompileColumnTable(owner *protocol.Reference, table *protocol.DeclaredTable) (*native.ColumnTableCreation, error) {
	if err := validateOwner(owner); err != nil {
		return nil, err
	}
	key, err := declaredKey(table)
	if err != nil {
		return nil, err
	}
	if err := only(table, "name", "schema", "primary_key", "primary_key_parts", "columns", "features"); err != nil {
		return nil, err
	}
	if table.Name != owner.Name || table.Schema != owner.Schema {
		return nil, fmt.Errorf("YDB table declaration has a different identity")
	}
	captured, err := tableFacets(owner, table.Features)
	if err != nil {
		return nil, err
	}
	if captured.storage == nil {
		return nil, fmt.Errorf("YDB column table declaration has no column-storage facet")
	}
	if captured.stated && !proto.Equal(captured.table, &native.RowTableIntent{}) {
		return nil, unsupported("%s", schema.ColumnTableRowSettingsRefusal)
	}
	result := &native.ColumnTableCreation{PrimaryKey: key, HashColumns: slices.Clone(captured.storage.HashColumns),
		Partitions: captured.storage.Partitions, Ttl: proto.CloneOf(captured.storage.Ttl)}
	if captured.storage.GetTtl().GetDisabled() != nil {
		result.Ttl = nil
	}
	if count := captured.storage.DataChannelCount; count != 0 && count != 64 {
		return nil, unsupported("a column table's data channel count is set by the server; the provider creates tables with its 64")
	}
	if len(captured.storage.LocalIndexes) != 0 {
		return nil, unsupported("the local indexes of a column table are not created by the provider yet")
	}
	var marked []string
	for _, column := range table.Columns {
		if column.GetPrimary() {
			marked = append(marked, column.GetName())
		}
	}
	if len(result.PrimaryKey) == 0 {
		if len(marked) != 1 {
			return nil, unsupported("YDB table creation requires an ordered primary key")
		}
		result.PrimaryKey = slices.Clone(marked)
	}
	names := make(map[string]bool, len(table.Columns))
	for _, column := range table.Columns {
		settings := captured.columns[column.GetName()]
		if settings != nil {
			if settings.GetDefaultValue() != nil || settings.Compression != nil || len(settings.Encoding) != 0 {
				return nil, unsupported("column tables do not support defaults or Serial columns")
			}
			if settings.Family != "" {
				return nil, unsupported("%s", schema.ColumnTableRowSettingsRefusal)
			}
		}
		// Every key column is written NOT NULL, as the host's embedded engine
		// writes it: YDB refuses a nullable key column in a column table, and a
		// Go field marked primary is declared nullable unless it says not_null.
		declared := column
		if slices.Contains(result.PrimaryKey, column.GetName()) && column.GetNullable() {
			declared = proto.CloneOf(column)
			declared.Nullable = false
		}
		compiled, err := compileColumn(declared, nil)
		if err != nil {
			return nil, err
		}
		if names[column.Name] {
			return nil, fmt.Errorf("duplicate YDB column")
		}
		names[column.Name] = true
		result.Columns = append(result.Columns, compiled)
	}
	for name := range captured.columns {
		if !names[name] {
			return nil, fmt.Errorf("YDB column properties name no declared column")
		}
	}
	if len(result.Columns) == 0 {
		return nil, unsupported("YDB table creation requires columns")
	}
	for _, key := range marked {
		if !slices.Contains(result.PrimaryKey, key) {
			return nil, fmt.Errorf("YDB column key membership disagrees with the ordered key")
		}
	}
	if err := ValidateColumnTableCreation(result); err != nil {
		return nil, err
	}
	return result, nil
}

// ValidateColumnTableCreation holds a creation to what CREATE TABLE writes and
// YDB keeps, so a refusal comes before the statement runs: a key of NOT NULL
// declared columns, hash columns among the key columns, and a TTL whose column
// is declared, first in the key (the host's rule: a later key column needs a
// local min_max index, which the provider does not create) and of a type its
// mode reads.
func ValidateColumnTableCreation(creation *native.ColumnTableCreation) error {
	if err := schema.Known(creation); err != nil {
		return err
	}
	if len(creation.Columns) == 0 || len(creation.PrimaryKey) == 0 {
		return fmt.Errorf("YDB creation requires columns and an ordered primary key")
	}
	types := map[string]string{}
	nullable := map[string]bool{}
	for _, column := range creation.Columns {
		if err := only(column, "name", "type", "not_null"); err != nil {
			return err
		}
		if !validName(column.Name) || types[column.Name] != "" {
			return fmt.Errorf("invalid or duplicate YDB creation column")
		}
		spelling, optional, err := schema.ColumnType(column.Type)
		if err != nil {
			return err
		}
		if column.NotNull == nil || *column.NotNull == optional {
			return fmt.Errorf("YDB column type and nullability disagree")
		}
		types[column.Name], nullable[column.Name] = spelling, optional
	}
	keys := map[string]bool{}
	for _, key := range creation.PrimaryKey {
		if types[key] == "" || keys[key] {
			return fmt.Errorf("invalid YDB primary key column")
		}
		if nullable[key] {
			return unsupported("column table key column %q must be NOT NULL; YDB refuses a nullable key column", key)
		}
		keys[key] = true
	}
	seen := map[string]bool{}
	for _, name := range creation.HashColumns {
		if !keys[name] {
			return unsupported("hash column %q must belong to the primary key", name)
		}
		if seen[name] {
			return fmt.Errorf("hash partitioning column %q is empty or repeated", name)
		}
		seen[name] = true
	}
	if creation.Partitions > 1<<32-1 {
		return fmt.Errorf("column shard count exceeds a 32-bit integer")
	}
	if creation.Ttl == nil {
		return nil
	}
	enabled := creation.Ttl.GetEnabled()
	if enabled == nil {
		return fmt.Errorf("a column table's creation states a TTL or none")
	}
	envelope := &native.ColumnStorageIntent{Ttl: creation.Ttl}
	payload, err := protojson.Marshal(envelope)
	if err != nil {
		return err
	}
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	if _, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "validation"}, Payload: payload}); err != nil {
		return err
	}
	ydbType, declared := types[enabled.Column]
	if !declared {
		return unsupported("the TTL reads column %q, which the table does not declare", enabled.Column)
	}
	if creation.PrimaryKey[0] != enabled.Column {
		return unsupported("TTL column %q must be first in the primary key or have a LOCAL min_max index", enabled.Column)
	}
	if reason := TTLColumnRefusal(enabled.Column, ydbType, enabled.ColumnUnit != 0); reason != "" {
		return unsupported("the TTL %s", reason)
	}
	return nil
}

// MovesRows reports whether a column table's TTL moves rows to an external data
// source, which requires TieredTTLCapability.
func MovesRows(ttl *native.ColumnTTLPolicy) bool {
	return slices.ContainsFunc(ttl.GetEnabled().GetTiers(), func(tier *native.ColumnTTLTier) bool { return tier.GetExternalStorage() != "" })
}

// tieredTTLRefusal says why a session cannot take a TTL that moves rows, or is
// empty. An unestablished capability refuses too: the session reads it from
// the cluster's feature flags only when the address names the monitoring
// endpoint.
func tieredTTLRefusal(capabilities map[string]bool, subject string, ttl *native.ColumnTTLPolicy) string {
	if !MovesRows(ttl) || capabilities[TieredTTLCapability] {
		return ""
	}
	return fmt.Sprintf("%s declares a TTL tier that moves rows to an external data source, which requires target capability %s, unavailable on this ydb target "+
		"(YDB keeps it behind EnableTieringInColumnShard; a session reads the flag when the address names the monitoring endpoint)", subject, TieredTTLCapability)
}

// CaptureColumnTable compiles a column table's declaration after its caller
// established the CREATE lifecycle.
func CaptureColumnTable(id string, owner *protocol.Reference, table *protocol.DeclaredTable) (*protocol.NormalizedValue, error) {
	creation, err := CompileColumnTable(owner, table)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(creation)
	if err != nil {
		return nil, err
	}
	definition := ColumnModel()
	return NormalizeColumnTable(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: ColumnOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// NormalizeColumnTable reconstructs the safety facts of a validated column-table
// creation.
func NormalizeColumnTable(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), ColumnModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB column table creation does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	creation, err := decodeColumnTable(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(creation)
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
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a column table",
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Creation inherits server access policy"}}, nil
}

// ColumnTable returns the creation an envelope carries after validating it.
func ColumnTable(value *protocol.Envelope) (*native.ColumnTableCreation, error) {
	normalized, err := NormalizeColumnTable(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeColumnTable(normalized.Value.Payload)
}

func decodeColumnTable(payload []byte) (*native.ColumnTableCreation, error) {
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
	var creation native.ColumnTableCreation
	if err := protojson.Unmarshal(canonical, &creation); err != nil {
		return nil, fmt.Errorf("invalid YDB column table creation JSON")
	}
	if err := ValidateColumnTableCreation(&creation); err != nil {
		return nil, err
	}
	return &creation, nil
}

// ColumnTableStatement writes the CREATE TABLE statement for a creation at an
// absolute path, in the form the host's embedded engine writes:
//
//	CREATE TABLE `path` (`c` Type NOT NULL, ..., PRIMARY KEY (`k`))
//	PARTITION BY HASH (`h`) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = n, TTL = ...)
//
// Intervals are written in seconds. The creation must be valid.
func ColumnTableStatement(path string, creation *native.ColumnTableCreation) string {
	var lines []string
	for _, column := range creation.Columns {
		spelling, _, _ := schema.ColumnType(column.Type)
		line := QuoteIdentifier(column.Name) + " " + spelling
		if column.GetNotNull() {
			line += " NOT NULL"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "PRIMARY KEY ("+quoteList(creation.PrimaryKey)+")")
	statement := "CREATE TABLE " + QuoteIdentifier(path) + " (\n    " + strings.Join(lines, ",\n    ") + "\n)"
	if len(creation.HashColumns) != 0 {
		statement += " PARTITION BY HASH (" + quoteList(creation.HashColumns) + ")"
	}
	settings := []string{"STORE = COLUMN"}
	if creation.Partitions != 0 {
		settings = append(settings, "AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = "+strconv.FormatUint(creation.Partitions, 10))
	}
	if creation.GetTtl().GetEnabled() != nil {
		settings = append(settings, "TTL = "+TTLClause(creation.Ttl))
	}
	return statement + " WITH (" + strings.Join(settings, ", ") + ");"
}

// TTLClause writes a column table's TTL as the value of YQL's TTL setting, as
// in `Interval("PT86400S") TO EXTERNAL DATA SOURCE `/local/ext`,
// Interval("PT604800S") DELETE ON `ts“.
func TTLClause(ttl *native.ColumnTTLPolicy) string {
	enabled := ttl.GetEnabled()
	parts := make([]string, 0, len(enabled.GetTiers()))
	for _, tier := range enabled.GetTiers() {
		part := `Interval("PT` + strconv.FormatUint(tier.ApplyAfterSeconds, 10) + `S")`
		if source := tier.GetExternalStorage(); source != "" {
			part += " TO EXTERNAL DATA SOURCE " + QuoteIdentifier(source)
		} else {
			part += " DELETE"
		}
		parts = append(parts, part)
	}
	clause := strings.Join(parts, ", ") + " ON " + QuoteIdentifier(enabled.GetColumn())
	if unit := ttlUnitNames[enabled.GetColumnUnit()]; unit != "" {
		clause += " AS " + unit
	}
	return clause
}

// ttlUnitNames spells a column TTL's epoch unit as YQL writes it after AS.
var ttlUnitNames = map[uint64]string{1: "SECONDS", 2: "MILLISECONDS", 3: "MICROSECONDS", 4: "NANOSECONDS"}

// QuoteIdentifier writes name as a YQL identifier in backticks, escaping a
// backtick and a backslash.
func QuoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), "`", "\\`") + "`"
}

func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = QuoteIdentifier(name)
	}
	return strings.Join(quoted, ", ")
}
