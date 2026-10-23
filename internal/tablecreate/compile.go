// Package tablecreate compiles captured declarations into native row-table requests.
package tablecreate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Compile preserves explicit key order and native settings. The result has no
// session or path: execution must bind both after checking the captured owner.
// Unknown or unimplemented populated fields refuse the complete declaration.
func Compile(owner *protocol.Reference, table *protocol.DeclaredTable) (*Ydb_Table.CreateTableRequest, error) {
	return compile(owner, table, true)
}

// compile is Compile with the policy only CREATE needs made optional. Access
// requirements and session-scoped tables are valid declarations that creation
// cannot establish, so validating the declaration alone leaves them be.
func compile(owner *protocol.Reference, table *protocol.DeclaredTable, creating bool) (*Ydb_Table.CreateTableRequest, error) {
	if err := validateOwner(owner); err != nil {
		return nil, err
	}
	if err := only(table, "name", "schema", "primary_key", "columns", "features"); err != nil {
		return nil, err
	}
	if table.Name != owner.Name || table.Schema != owner.Schema {
		return nil, fmt.Errorf("YDB table declaration has a different identity")
	}
	settings, columns, err := features(owner, table.Features)
	if err != nil {
		return nil, err
	}
	if creating && settings.Access != nil {
		return nil, fmt.Errorf("YDB table creation cannot establish access requirements before creation")
	}
	if creating && settings.Temporary {
		return nil, fmt.Errorf("temporary YDB tables need an explicit session lifetime")
	}
	result := &Ydb_Table.CreateTableRequest{
		PrimaryKey:           slices.Clone(table.PrimaryKey),
		StoreType:            Ydb_Table.StoreType_STORE_TYPE_ROW,
		TtlSettings:          proto.CloneOf(settings.TtlSettings),
		StorageSettings:      proto.CloneOf(settings.StorageSettings),
		Attributes:           maps.Clone(settings.Attributes),
		PartitioningSettings: proto.CloneOf(settings.PartitioningSettings),
		KeyBloomFilter:       settings.KeyBloomFilter,
		ReadReplicasSettings: proto.CloneOf(settings.ReadReplicasSettings),
		Tiering:              settings.Tiering,
	}
	for _, family := range settings.ColumnFamilies {
		result.ColumnFamilies = append(result.ColumnFamilies, proto.CloneOf(family))
	}
	if len(settings.PrimaryKey) > 0 {
		if len(result.PrimaryKey) != 0 && !slices.Equal(result.PrimaryKey, settings.PrimaryKey) {
			return nil, fmt.Errorf("common and native YDB primary key orders disagree")
		}
		result.PrimaryKey = slices.Clone(settings.PrimaryKey)
	}
	names := make(map[string]bool, len(table.Columns))
	var marked []string
	for _, column := range table.Columns {
		compiled, err := compileColumn(column, columns[column.GetName()])
		if err != nil {
			return nil, err
		}
		if names[column.Name] {
			return nil, fmt.Errorf("duplicate YDB column")
		}
		names[column.Name] = true
		if column.Primary {
			marked = append(marked, column.Name)
		}
		result.Columns = append(result.Columns, compiled)
	}
	if len(result.Columns) == 0 {
		return nil, unsupported("YDB table creation requires columns")
	}
	for name := range columns {
		if !names[name] {
			return nil, fmt.Errorf("YDB column properties name no declared column")
		}
	}
	if len(result.PrimaryKey) == 0 {
		if len(marked) != 1 {
			return nil, unsupported("YDB table creation requires an ordered primary key")
		}
		result.PrimaryKey = slices.Clone(marked)
	}
	keys := make(map[string]bool, len(result.PrimaryKey))
	for _, key := range result.PrimaryKey {
		if !names[key] || keys[key] {
			return nil, fmt.Errorf("invalid YDB primary key column")
		}
		keys[key] = true
	}
	for _, key := range marked {
		if !keys[key] {
			return nil, fmt.Errorf("YDB column key membership disagrees with the ordered key")
		}
	}
	if err := validateReferences(result, names); err != nil {
		return nil, err
	}
	return result, nil
}

func compileColumn(column *protocol.DeclaredColumn, settings *native.ColumnIntent) (*Ydb_Table.ColumnMeta, error) {
	if err := only(column, "name", "type", "type_raw_sql", "type_is_declared_text", "nullable", "primary"); err != nil {
		return nil, err
	}
	if !validName(column.Name) {
		return nil, fmt.Errorf("invalid YDB column name")
	}
	value, err := schema.StorageType(column.Type, column.Nullable)
	if err != nil {
		return nil, unsupported("%s", err)
	}
	result := &Ydb_Table.ColumnMeta{Name: column.Name, Type: value, NotNull: new(!column.Nullable)}
	if settings == nil {
		return result, nil
	}
	if err := only(settings, "family", "literal", "drop_default"); err != nil {
		return nil, err
	}
	result.Family = settings.Family
	if literal := settings.GetLiteral(); literal != nil {
		// A literal is a typed native value, never SQL text. Type conversion
		// belongs to the server; require an exact type to avoid implicit casts.
		if !proto.Equal(literal.Type, value) {
			return nil, fmt.Errorf("YDB literal default type differs from the column type")
		}
		result.DefaultValue = &Ydb_Table.ColumnMeta_FromLiteral{FromLiteral: proto.CloneOf(literal)}
	}
	return result, nil
}

func validateReferences(request *Ydb_Table.CreateTableRequest, names map[string]bool) error {
	for _, name := range request.PartitioningSettings.GetPartitionBy() {
		if !names[name] {
			return fmt.Errorf("YDB partitioning names no declared column")
		}
	}
	if ttl := request.TtlSettings; ttl != nil {
		name := ttl.GetDateTypeColumn().GetColumnName()
		if name == "" {
			name = ttl.GetValueSinceUnixEpoch().GetColumnName()
		}
		if !names[name] {
			return fmt.Errorf("YDB TTL names no declared column")
		}
	}
	families := map[string]bool{"default": true}
	for _, family := range request.ColumnFamilies {
		families[family.Name] = true
	}
	for _, column := range request.Columns {
		if column.Family != "" && !families[column.Family] {
			return fmt.Errorf("YDB column names no declared family")
		}
	}
	return nil
}

func only(value proto.Message, allowed ...string) error {
	if err := schema.Known(value); err != nil {
		return err
	}
	var result error
	value.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if !slices.Contains(allowed, string(field.Name())) {
			result = unsupported("YDB table creation does not support %s", field.FullName())
		}
		return result == nil
	})
	return result
}

func validName(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00/\\") && value != "." && value != ".."
}

func validateOwner(owner *protocol.Reference) error {
	if err := only(owner, "engine", "kind", "database", "schema", "name"); err != nil {
		return err
	}
	if owner.Engine != "ydb" || owner.Kind != "table" || !validName(owner.Name) || (owner.Schema == "" && owner.Name == "ptah_locks") {
		return fmt.Errorf("invalid YDB table owner")
	}
	for _, part := range strings.Split(owner.Schema, "/") {
		if owner.Schema != "" && !validName(part) {
			return fmt.Errorf("invalid YDB table namespace")
		}
	}
	if owner.Database != "" {
		if !strings.HasPrefix(owner.Database, "/") || owner.Database == "/" {
			return fmt.Errorf("invalid YDB database path")
		}
		for _, part := range strings.Split(owner.Database[1:], "/") {
			if !validName(part) {
				return fmt.Errorf("invalid YDB database path")
			}
		}
	}
	return nil
}
