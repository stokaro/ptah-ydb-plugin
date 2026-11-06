// Package ownedtable validates provider-owned native table storage.
package ownedtable

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Identity keeps the selected database, relative directory, and table separate.
type Identity struct{ Database, Directory, Name string }

// Resolve refuses traversal rather than cleaning it into a different store.
func Resolve(database, directory, name string) (Identity, error) {
	if !path.IsAbs(database) || path.Clean(database) != database || !utf8.ValidString(database) || strings.ContainsAny(database, "\\\x00") || !component(name) {
		return Identity{}, fmt.Errorf("invalid YDB store identity")
	}
	if directory == "/" {
		directory = ""
	}
	if directory != "" {
		for part := range strings.SplitSeq(directory, "/") {
			if !component(part) {
				return Identity{}, fmt.Errorf("invalid YDB store directory")
			}
		}
	}
	return Identity{Database: database, Directory: directory, Name: name}, nil
}
func component(value string) bool {
	return utf8.ValidString(value) && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

// Path returns the absolute native path of the selected store.
func (s Identity) Path() string { return path.Join(s.Database, s.Directory, s.Name) }

// QuotedPath quotes the whole native path using YQL backslash escapes.
func (s Identity) QuotedPath() string {
	return "`" + strings.ReplaceAll(strings.ReplaceAll(s.Path(), "\\", "\\\\"), "`", "\\`") + "`"
}

// Column names a native primitive storage column, nonnullable unless
// Nullable says so.
type Column struct {
	Name string
	Type Ydb.Type_PrimitiveTypeId
	// Nullable reports a column that holds NULL: an optional of Type with no
	// NOT NULL. Only a layout another program wrote has one (see Definition).
	Nullable bool
}

// Definition binds an exact native layout to one ownership marker. A
// definition with no Attribute is the layout of a table that carries no
// attribute at all: the stores Ptah's built-in YDB engine wrote, which a
// provider reads and writes in their own layout rather than create.
type Definition struct {
	Identity
	Attribute, Marker string
	Columns           []Column
	PrimaryKey        []string
}

// CreateRequest installs the layout and marker in the same native operation.
func (s Definition) CreateRequest() *Ydb_Table.CreateTableRequest {
	result := &Ydb_Table.CreateTableRequest{Path: s.Path(), StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW, PrimaryKey: slices.Clone(s.PrimaryKey), Attributes: map[string]string{s.Attribute: s.Marker}}
	for _, column := range s.Columns {
		result.Columns = append(result.Columns, &Ydb_Table.ColumnMeta{Name: column.Name, Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: column.Type}}, NotNull: new(true)})
	}
	return result
}

// ValidateLayout refuses foreign ownership, column drift, and automatic behavior.
// Physical partition and storage tuning does not change the record contract.
func (s Definition) ValidateLayout(value *native.DescribeTableResult) error {
	if err := schema.Known(value); err != nil {
		return err
	}
	if !s.owns(value) {
		return fmt.Errorf("YDB storage ownership or table kind does not match")
	}
	if !s.plainLayout(value) {
		return fmt.Errorf("YDB storage has incompatible layout or automatic behavior")
	}
	return s.validateColumns(value)
}

// owns reports whether the described entry is a row table of the definition's
// name whose only attribute is the definition's ownership marker, or that
// carries no attribute for a definition with none.
func (s Definition) owns(value *native.DescribeTableResult) bool {
	rowStore := value.StoreType == Ydb_Table.StoreType_STORE_TYPE_ROW || value.StoreType == Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED
	marked := len(value.Attributes) == 1 && value.Attributes[s.Attribute] == s.Marker
	if s.Attribute == "" {
		marked = len(value.Attributes) == 0
	}
	return value.GetSelf().GetName() == s.Name && value.GetSelf().GetType() == Ydb_Scheme.Entry_TABLE && rowStore && marked
}

// plainLayout reports whether the table has the definition's column count and
// primary key, and no index, changefeed, TTL, tiering or temporary lifetime.
func (s Definition) plainLayout(value *native.DescribeTableResult) bool {
	return len(value.Columns) == len(s.Columns) && slices.Equal(value.PrimaryKey, s.PrimaryKey) && len(value.Indexes) == 0 &&
		len(value.Changefeeds) == 0 && value.TtlSettings == nil && value.Tiering == "" && !value.Temporary
}

// validateColumns refuses a described column the definition does not declare
// with the same primitive type and nullability, a column with a default or a
// non-default family, and a declared column the table lacks.
func (s Definition) validateColumns(value *native.DescribeTableResult) error {
	expected := make(map[string]Column, len(s.Columns))
	for _, column := range s.Columns {
		expected[column.Name] = column
	}
	for _, column := range value.Columns {
		wanted, ok := expected[column.Name]
		// A primitive type is nonnullable. Optional native types may carry the
		// explicit NOT NULL bit; compare their decoded primitive representation.
		actual, nullable := column.Type, false
		if optional := actual.GetOptionalType(); optional != nil {
			actual, nullable = optional.Item, !column.GetNotNull()
		}
		if !ok || actual.GetTypeId() != wanted.Type || nullable != wanted.Nullable || column.DefaultValue != nil ||
			(column.Family != "" && column.Family != "default") {
			return fmt.Errorf("YDB storage has an incompatible column")
		}
		delete(expected, column.Name)
	}
	if len(expected) != 0 {
		return fmt.Errorf("YDB storage is missing required columns")
	}
	return nil
}

// Check establishes absence or validates the complete native layout.
func (s Definition) Check(ctx context.Context, driver *ydb.Driver) (bool, error) {
	return s.Identity.Check(ctx, driver, s.ValidateLayout)
}

// Prepare creates absent storage once and verifies its native description.
func (s Definition) Prepare(ctx context.Context, driver *ydb.Driver, beforeCreate func(context.Context) error) (bool, error) {
	return s.Identity.Prepare(ctx, driver, s.CreateRequest(), s.ValidateLayout, beforeCreate)
}
