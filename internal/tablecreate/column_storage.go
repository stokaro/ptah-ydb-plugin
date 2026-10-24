package tablecreate

import (
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// LayoutSatisfied reports whether a column table holding held has the layout
// the declaration asks for: the hash key and the shard count where the
// declaration states them. A declaration that leaves one out takes what the
// table holds. YDB fixes both when it creates the table.
func LayoutSatisfied(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation) bool {
	return (desired.Partitions == 0 || desired.Partitions == held.Partitions) &&
		(len(desired.HashColumns) == 0 || slices.Equal(desired.HashColumns, held.HashColumns))
}

// ColumnTTLEqual reports whether two column-table TTLs keep and move the same
// rows: the same column, unit and tiers, each with its seconds and action. A
// disabled TTL is no TTL, and the expiration the server derives from the tiers
// is not compared.
func ColumnTTLEqual(desired, held *native.ColumnTTLPolicy) bool {
	a, b := desired.GetEnabled(), held.GetEnabled()
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Column == b.Column && a.ColumnUnit == b.ColumnUnit && slices.EqualFunc(a.Tiers, b.Tiers, func(x, y *native.ColumnTTLTier) bool { return proto.Equal(x, y) })
}

// localIndexesDiffer reports whether a declaration states local indexes the
// table does not hold exactly. A declaration that states none leaves the
// table's local indexes alone: no source declares them yet.
func localIndexesDiffer(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation) bool {
	if len(desired.LocalIndexes) == 0 {
		return false
	}
	return !slices.EqualFunc(desired.LocalIndexes, held.LocalIndexes, func(a, b *native.LocalIndexObservation) bool { return proto.Equal(a, b) })
}

// ColumnStorageDiffers reports whether a column table holding held does not
// already satisfy the declared column storage: another hash key or shard count
// the declaration states, another TTL, or local indexes the declaration states
// differently.
func ColumnStorageDiffers(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation) bool {
	return !LayoutSatisfied(desired, held) || !ColumnTTLEqual(desired.Ttl, held.Ttl) || localIndexesDiffer(desired, held)
}

// StorageFacetDiffers applies ColumnStorageDiffers to a validated desired and
// observed column-storage facet of one owner.
func StorageFacetDiffers(desired, observed *protocol.Envelope) (bool, error) {
	if desired.GetKind() != schema.ColumnStorageProperties || observed.GetKind() != schema.ColumnStorageProperties {
		return false, fmt.Errorf("YDB column storage comparison requires column-storage facets")
	}
	var want native.ColumnStorageIntent
	if err := protojson.Unmarshal(desired.Payload, &want); err != nil {
		return false, err
	}
	var held native.ColumnStorageObservation
	if err := protojson.Unmarshal(observed.Payload, &held); err != nil {
		return false, err
	}
	return ColumnStorageDiffers(&want, &held), nil
}
