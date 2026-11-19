// Package repeatables owns the YDB storage of the record of repeatable
// migrations: one row per identity, the encoded row the host reads and the
// bytes of the content that last applied. The host owns what the record
// means; this package keeps its layout, its exact row codec and the atomic
// write of one row.
package repeatables

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ownershipAttribute is the table attribute that marks the record, with
// marker as its value. A table without it is never the record.
const (
	ownershipAttribute = "ptah.repeatable"
	marker             = "stokaro/ptah-ydb-plugin/repeatable-v1"
)

// maxRowBytes bounds one encoded row: every field of a row is bounded by the
// SDK, so a longer payload is not one this store wrote.
const maxRowBytes = 64 << 10

// Store binds the record's identity to a database.
type Store struct{ ownedtable.Definition }

// Resolve validates the scope and binds its schema and table, refusing
// traversal rather than cleaning it into another store.
func Resolve(database string, scope *protocol.RepeatableScope) (Store, error) {
	if err := provider.ValidateRepeatableScope(scope); err != nil {
		return Store{}, err
	}
	identity, err := ownedtable.Resolve(database, scope.GetSchema(), scope.GetTable())
	if err != nil {
		return Store{}, err
	}
	return definition(identity), nil
}

func definition(identity ownedtable.Identity) Store {
	return Store{ownedtable.Definition{
		Identity:  identity,
		Attribute: ownershipAttribute,
		Marker:    marker,
		Columns: []ownedtable.Column{
			{Name: "identity", Type: Ydb.Type_UTF8},
			{Name: "row", Type: Ydb.Type_STRING},
			{Name: "body", Type: Ydb.Type_STRING},
		},
		PrimaryKey: []string{"identity"},
	}}
}

func prefix() []byte { return []byte(marker + "\x00") }

// Encode is the stored form of row: the marker and the row's deterministic
// encoding. It refuses a row the SDK refuses and a field this build does not
// know, so nothing it stores is lost on the way back.
func Encode(row *protocol.RepeatableRow) ([]byte, error) {
	if err := schema.Known(row); err != nil {
		return nil, err
	}
	if err := provider.ValidateRepeatableRow(row); err != nil {
		return nil, err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(row)
	if err != nil {
		return nil, err
	}
	payload := append(prefix(), encoded...)
	if len(payload) > maxRowBytes {
		return nil, fmt.Errorf("YDB repeatable migration row exceeds its size limit")
	}
	return payload, nil
}

// Decode reads a stored row and refuses one that is not exactly what Encode
// writes for identity.
func Decode(identity string, payload []byte) (*protocol.RepeatableRow, error) {
	if len(payload) > maxRowBytes || !bytes.HasPrefix(payload, prefix()) {
		return nil, fmt.Errorf("incompatible YDB repeatable migration row encoding")
	}
	row := &protocol.RepeatableRow{}
	if err := proto.Unmarshal(payload[len(prefix()):], row); err != nil {
		return nil, fmt.Errorf("invalid YDB repeatable migration row")
	}
	canonical, err := Encode(row)
	if err != nil {
		return nil, err
	}
	if row.GetIdentity() != identity || !bytes.Equal(canonical, payload) {
		return nil, fmt.Errorf("YDB repeatable migration row identity or encoding is not canonical")
	}
	return row, nil
}

// Locate establishes the record's absence, or describes it and refuses a
// table of another owner or layout.
func (s Store) Locate(ctx context.Context, driver *ydb.Driver) (bool, error) {
	return s.Check(ctx, driver)
}

// Prepare creates the record once, with its marker in the same native
// request, and confirms the layout of an existing one. It never adopts an
// unmarked table or changes a row. attempted reports a failure after the
// create was sent.
func (s Store) Prepare(ctx context.Context, driver *ydb.Driver, beforeCreate func(context.Context) error) (attempted bool, err error) {
	return s.Definition.Prepare(ctx, driver, beforeCreate)
}

// Owned identifies the record by its exact marker and complete native layout,
// never by its name. A table that carries the attribute with another value,
// or the marker over another layout, refuses inspection rather than being
// read as a table of the schema.
func Owned(name string, value *native.DescribeTableResult) (bool, error) {
	found, exists := value.GetAttributes()[ownershipAttribute]
	if !exists {
		return false, nil
	}
	if found != marker {
		return false, fmt.Errorf("unknown YDB repeatable migration record marker")
	}
	if err := definition(ownedtable.Identity{Name: name}).ValidateLayout(value); err != nil {
		return false, err
	}
	return true, nil
}
