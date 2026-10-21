// Package history owns the YDB revision storage format and exact row codec.
package history

import (
	"bytes"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// MaxSnapshotBytes leaves room for RPC framing and scope metadata.
const MaxSnapshotBytes = 3 << 20
const ownershipAttribute = "ptah.history"

// Store binds exact storage identity and representation to a database.
type Store struct {
	Database  string
	Directory string
	Name      string
	Format    protocol.HistoryScope_Format
}

// Resolve refuses namespace traversal instead of cleaning it into another store.
func Resolve(database string, scope *protocol.HistoryScope) (Store, error) {
	if err := provider.ValidateHistoryScope(scope); err != nil {
		return Store{}, err
	}
	if !path.IsAbs(database) || path.Clean(database) != database || !component(scope.Table) {
		return Store{}, fmt.Errorf("invalid YDB history store identity")
	}
	directory := scope.Schema
	if directory == "/" {
		directory = ""
	}
	if directory != "" {
		for _, part := range strings.Split(directory, "/") {
			if !component(part) {
				return Store{}, fmt.Errorf("invalid YDB history directory")
			}
		}
	}
	return Store{Database: database, Directory: directory, Name: scope.Table, Format: scope.Format}, nil
}
func component(value string) bool {
	return utf8.ValidString(value) && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

// Path returns the absolute native path of the selected store.
func (s Store) Path() string { return path.Join(s.Database, s.Directory, s.Name) }

// QuotedPath quotes the whole native path using YQL backslash escapes.
func (s Store) QuotedPath() string {
	return "`" + strings.ReplaceAll(strings.ReplaceAll(s.Path(), "\\", "\\\\"), "`", "\\`") + "`"
}
func (s Store) marker() string {
	return "stokaro/ptah-ydb-plugin/history-v1/" + strings.ToLower(s.Format.String())
}
func (s Store) prefix() []byte { return []byte(s.marker() + "\x00") }

// KeyType names the native key type; native identities remain signed integers.
func (s Store) KeyType() string {
	if s.Format == protocol.HistoryScope_NATIVE {
		return "Int64"
	}
	return "Utf8"
}

// Key binds a validated version as a typed query parameter.
func (s Store) Key(version string) (any, error) {
	if s.Format == protocol.HistoryScope_NATIVE {
		value, err := strconv.ParseInt(version, 10, 64)
		if err != nil || strconv.FormatInt(value, 10) != version {
			return nil, fmt.Errorf("invalid native revision identity")
		}
		return value, nil
	}
	if !utf8.ValidString(version) {
		return nil, fmt.Errorf("invalid Atlas revision identity")
	}
	return version, nil
}

// Encode preserves every stored fact without timestamp or duration truncation.
func (s Store) Encode(row *protocol.HistoryRevision) ([]byte, error) {
	if err := schema.Known(row); err != nil {
		return nil, err
	}
	if err := provider.ValidateHistoryRevision(s.Format, row); err != nil {
		return nil, err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(row)
	if err != nil {
		return nil, err
	}
	payload := append(s.prefix(), encoded...)
	if len(payload) > MaxSnapshotBytes {
		return nil, fmt.Errorf("YDB revision exceeds snapshot size limit")
	}
	return payload, nil
}

// Decode refuses unknown or noncanonical records before exposing any history.
func (s Store) Decode(version string, payload []byte) (*protocol.HistoryRevision, error) {
	prefix := s.prefix()
	if !bytes.HasPrefix(payload, prefix) || len(payload) > MaxSnapshotBytes {
		return nil, fmt.Errorf("incompatible YDB revision encoding")
	}
	row := &protocol.HistoryRevision{}
	if err := proto.Unmarshal(payload[len(prefix):], row); err != nil {
		return nil, fmt.Errorf("invalid YDB revision record")
	}
	canonical, err := s.Encode(row)
	if err != nil {
		return nil, err
	}
	if row.Version != version || !bytes.Equal(canonical, payload) {
		return nil, fmt.Errorf("YDB revision identity or encoding is not canonical")
	}
	return row, nil
}
func (s Store) nativeKeyType() Ydb.Type_PrimitiveTypeId {
	if s.Format == protocol.HistoryScope_NATIVE {
		return Ydb.Type_INT64
	}
	return Ydb.Type_UTF8
}
func primitive(kind Ydb.Type_PrimitiveTypeId) *Ydb.Type {
	return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: kind}}
}

// CreateRequest declares the layout and ownership marker in the same operation.
func (s Store) CreateRequest() *Ydb_Table.CreateTableRequest {
	return &Ydb_Table.CreateTableRequest{
		Path: s.Path(), StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW,
		Columns: []*Ydb_Table.ColumnMeta{
			{Name: "version", Type: primitive(s.nativeKeyType()), NotNull: proto.Bool(true)},
			{Name: "revision", Type: primitive(Ydb.Type_STRING), NotNull: proto.Bool(true)},
		}, PrimaryKey: []string{"version"}, Attributes: map[string]string{ownershipAttribute: s.marker()},
	}
}

// ValidateLayout refuses unowned stores and schema behavior that could remove
// history or change a mutation. Physical partition/storage tuning is retained.
func (s Store) ValidateLayout(value *native.DescribeTableResult) error {
	if err := schema.Known(value); err != nil {
		return err
	}
	if value.GetSelf().GetName() != s.Name || value.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE ||
		(value.StoreType != Ydb_Table.StoreType_STORE_TYPE_ROW && value.StoreType != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED) ||
		len(value.Attributes) != 1 || value.Attributes[ownershipAttribute] != s.marker() {
		return fmt.Errorf("YDB history ownership or table kind does not match")
	}
	if len(value.Columns) != 2 || len(value.PrimaryKey) != 1 || value.PrimaryKey[0] != "version" ||
		len(value.Indexes) != 0 || len(value.Changefeeds) != 0 || value.TtlSettings != nil || value.Tiering != "" || value.Temporary {
		return fmt.Errorf("YDB history has incompatible layout or automatic behavior")
	}
	expected := map[string]string{"version": s.KeyType(), "revision": "String"}
	for _, column := range value.Columns {
		spelling, nullable, err := schema.ColumnType(column.Type)
		if column.GetNotNull() {
			nullable = false
		}
		if err != nil || nullable || expected[column.Name] != spelling || column.DefaultValue != nil ||
			(column.Family != "" && column.Family != "default") {
			return fmt.Errorf("YDB history has an incompatible column")
		}
		delete(expected, column.Name)
	}
	if len(expected) != 0 {
		return fmt.Errorf("YDB history is missing required columns")
	}
	return nil
}

// Owned identifies history by its exact marker and complete native layout, never
// by a table name. Unknown markers and altered marked tables refuse inspection.
func Owned(name string, value *native.DescribeTableResult) (bool, error) {
	marker, exists := value.GetAttributes()[ownershipAttribute]
	if !exists {
		return false, nil
	}
	for _, format := range []protocol.HistoryScope_Format{protocol.HistoryScope_NATIVE, protocol.HistoryScope_ATLAS} {
		store := Store{Name: name, Format: format}
		if marker == store.marker() {
			if err := store.ValidateLayout(value); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, fmt.Errorf("unknown YDB history ownership marker")
}
