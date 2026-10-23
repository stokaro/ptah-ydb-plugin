package tablecreate

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
)

// NormalizedFacet re-normalizes the value a comparison or planning record
// carries and requires the host's receipt to match. It returns the validated
// envelope of a table or column facet.
func NormalizedFacet(record *protocol.ComparisonRecord, representation protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	return normalizedFacet(record, representation)
}

// FacetDiffers reports whether the table the observed facet describes does not
// already satisfy the desired facet. It applies the rules the alteration
// planner applies, so a difference reported here is one the planner can act on:
// the TTL and the column families of a table facet, and the family a column
// sits in for a column facet. Both envelopes must be validated and belong to one
// owner.
func FacetDiffers(desired, observed *protocol.Envelope) (bool, error) {
	if desired.GetKind() != observed.GetKind() {
		return false, fmt.Errorf("YDB facet comparison requires one model")
	}
	switch desired.GetKind() {
	case schema.TableProperties:
		var want native.RowTableIntent
		if err := protojson.Unmarshal(desired.Payload, &want); err != nil {
			return false, err
		}
		var held native.DescribeTableResult
		if err := protojson.Unmarshal(observed.Payload, &held); err != nil {
			return false, err
		}
		return tableDiffers(&want, &held), nil
	case schema.ColumnProperties:
		var want native.ColumnIntent
		if err := protojson.Unmarshal(desired.Payload, &want); err != nil {
			return false, err
		}
		var held native.ColumnMeta
		if err := protojson.Unmarshal(observed.Payload, &held); err != nil {
			return false, err
		}
		return familyKey(want.Family) != familyKey(held.Family), nil
	default:
		return false, fmt.Errorf("YDB facet comparison does not support %s", desired.GetKind())
	}
}

// tableDiffers applies the TTL and family rules to the table properties. A
// declaration that states table properties manages the TTL, so one without a TTL
// differs from a table that holds one.
func tableDiffers(want *native.RowTableIntent, held *native.DescribeTableResult) bool {
	if want.TtlSettings == nil && held.TtlSettings != nil || want.TtlSettings != nil && !sameTTL(want.TtlSettings, held.TtlSettings) {
		return true
	}
	for _, family := range want.ColumnFamilies {
		current := findFamily(held.ColumnFamilies, family.GetName())
		if current == nil {
			return true
		}
		if data, compression := familyDelta(family, current); data || compression {
			return true
		}
		if family.GetKeepInMemory() == Ydb.FeatureFlag_ENABLED && current.GetKeepInMemory() != Ydb.FeatureFlag_ENABLED {
			return true
		}
	}
	return false
}
