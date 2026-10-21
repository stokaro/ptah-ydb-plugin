// Package metadata owns exact YDB migration journal and tag storage.
package metadata

import (
	"bytes"
	"fmt"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"
)

const ownershipAttribute = "ptah.metadata"

// Store binds the selected log or tag format to an exact native table.
type Store struct {
	ownedtable.Definition
	Kind protocol.MetadataScope_Kind
}

// Resolve validates the storage scope without rewriting path components.
func Resolve(database string, scope *protocol.MetadataScope) (Store, error) {
	if err := provider.ValidateMetadataScope(scope); err != nil {
		return Store{}, err
	}
	identity, err := ownedtable.Resolve(database, scope.Schema, scope.Table)
	if err != nil {
		return Store{}, err
	}
	return definition(identity, scope.Kind), nil
}
func definition(identity ownedtable.Identity, kind protocol.MetadataScope_Kind) Store {
	d := ownedtable.Definition{Identity: identity, Attribute: ownershipAttribute, Marker: "stokaro/ptah-ydb-plugin/metadata-v1/" + strings.ToLower(kind.String()), Columns: []ownedtable.Column{{Name: "name", Type: Ydb.Type_UTF8}, {Name: "record", Type: Ydb.Type_STRING}}, PrimaryKey: []string{"name"}}
	if kind == protocol.MetadataScope_LOG {
		d.Columns = []ownedtable.Column{{Name: "run_id", Type: Ydb.Type_UTF8}, {Name: "sequence", Type: Ydb.Type_INT64}, {Name: "record", Type: Ydb.Type_STRING}}
		d.PrimaryKey = []string{"run_id", "sequence"}
	}
	return Store{Definition: d, Kind: kind}
}
func (s Store) prefix() []byte { return []byte(s.Marker + "\x00") }

// Encode retains every SDK record field and refuses unknown schema fields.
func (s Store) Encode(row proto.Message) ([]byte, error) {
	if err := schema.Known(row); err != nil {
		return nil, err
	}
	var err error
	switch value := row.(type) {
	case *protocol.MigrationLogRecord:
		if s.Kind != protocol.MetadataScope_LOG {
			return nil, fmt.Errorf("record does not match YDB metadata kind")
		}
		err = provider.ValidateMigrationLogRecord(value)
	case *protocol.MigrationTagRecord:
		if s.Kind != protocol.MetadataScope_TAGS {
			return nil, fmt.Errorf("record does not match YDB metadata kind")
		}
		err = provider.ValidateMigrationTagRecord(value)
	default:
		return nil, fmt.Errorf("unsupported YDB metadata record")
	}
	if err != nil {
		return nil, err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(row)
	if err != nil {
		return nil, err
	}
	payload := append(s.prefix(), encoded...)
	if len(payload) > provider.MaxMetadataBytes {
		return nil, fmt.Errorf("YDB metadata record exceeds byte limit")
	}
	return payload, nil
}

// Decode accepts only this format's complete canonical record representation.
func (s Store) Decode(payload []byte) (proto.Message, error) {
	if len(payload) > provider.MaxMetadataBytes || !bytes.HasPrefix(payload, s.prefix()) {
		return nil, fmt.Errorf("incompatible YDB metadata encoding")
	}
	var row proto.Message = &protocol.MigrationTagRecord{}
	if s.Kind == protocol.MetadataScope_LOG {
		row = &protocol.MigrationLogRecord{}
	}
	if err := proto.Unmarshal(payload[len(s.prefix()):], row); err != nil {
		return nil, fmt.Errorf("invalid YDB metadata record")
	}
	canonical, err := s.Encode(row)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, payload) {
		return nil, fmt.Errorf("noncanonical YDB metadata record")
	}
	return row, nil
}

// Owned recognizes a store only from its exact marker and full native layout.
// Unknown marker versions and altered marked tables refuse inspection.
func Owned(name string, value *native.DescribeTableResult) (protocol.ReservedObject_Purpose, error) {
	marker, exists := value.GetAttributes()[ownershipAttribute]
	if !exists {
		return protocol.ReservedObject_PURPOSE_UNSPECIFIED, nil
	}
	for _, kind := range []protocol.MetadataScope_Kind{protocol.MetadataScope_LOG, protocol.MetadataScope_TAGS} {
		store := definition(ownedtable.Identity{Name: name}, kind)
		if marker == store.Marker {
			if err := store.ValidateLayout(value); err != nil {
				return 0, err
			}
			if kind == protocol.MetadataScope_LOG {
				return protocol.ReservedObject_LOG, nil
			}
			return protocol.ReservedObject_TAG, nil
		}
	}
	return 0, fmt.Errorf("unknown YDB metadata ownership marker")
}
