// Package history owns the YDB revision storage format and exact row codec.
package history

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// MaxSnapshotBytes leaves room for RPC framing and scope metadata.
const MaxSnapshotBytes = 3 << 20
const ownershipAttribute = "ptah.history"

// Store binds exact storage identity and representation to a database.
type Store struct {
	ownedtable.Identity
	Format protocol.HistoryScope_Format
}

// Resolve refuses namespace traversal instead of cleaning it into another store.
func Resolve(database string, scope *protocol.HistoryScope) (Store, error) {
	if err := provider.ValidateHistoryScope(scope); err != nil {
		return Store{}, err
	}
	identity, err := ownedtable.Resolve(database, scope.Schema, scope.Table)
	if err != nil {
		return Store{}, err
	}
	return Store{Identity: identity, Format: scope.Format}, nil
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
func (s Store) definition() ownedtable.Definition {
	return ownedtable.Definition{Identity: s.Identity, Attribute: ownershipAttribute, Marker: s.marker(), Columns: []ownedtable.Column{{Name: "version", Type: s.nativeKeyType()}, {Name: "revision", Type: Ydb.Type_STRING}}, PrimaryKey: []string{"version"}}
}

// CreateRequest installs the history marker and native layout together.
func (s Store) CreateRequest() *Ydb_Table.CreateTableRequest { return s.definition().CreateRequest() }

// ValidateLayout refuses foreign or altered history storage.
func (s Store) ValidateLayout(value *native.DescribeTableResult) error {
	return s.definition().ValidateLayout(value)
}

// Check establishes history absence or validates its owned storage.
func (s Store) Check(ctx context.Context, driver *ydb.Driver) (bool, error) {
	return s.definition().Check(ctx, driver)
}

// Prepare creates absent history storage once and confirms its layout.
func (s Store) Prepare(ctx context.Context, driver *ydb.Driver, beforeCreate func(context.Context) error) (bool, error) {
	return s.definition().Prepare(ctx, driver, beforeCreate)
}

// Owned identifies history by its exact marker and complete native layout, never
// by a table name. Unknown markers and altered marked tables refuse inspection.
func Owned(name string, value *native.DescribeTableResult) (bool, error) {
	marker, exists := value.GetAttributes()[ownershipAttribute]
	if !exists {
		return false, nil
	}
	for _, format := range []protocol.HistoryScope_Format{protocol.HistoryScope_NATIVE, protocol.HistoryScope_ATLAS} {
		store := Store{Identity: ownedtable.Identity{Name: name}, Format: format}
		if marker == store.marker() {
			if err := store.ValidateLayout(value); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, fmt.Errorf("unknown YDB history ownership marker")
}
