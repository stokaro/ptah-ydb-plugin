package tablecreate

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NormalizedFacet re-normalizes the value a comparison or planning record
// carries and requires the host's receipt to match. It returns the validated
// envelope of a table or column facet.
func NormalizedFacet(record *protocol.ComparisonRecord, representation protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	return normalizedFacet(record, representation)
}

// UnstatedTableFacet returns the desired table properties of a table whose
// source states none: the intent with no setting, which the comparison reads as
// a request for no TTL.
func UnstatedTableFacet(owner *protocol.Reference) (*protocol.Envelope, error) {
	payload, err := protojson.Marshal(&native.RowTableIntent{})
	if err != nil {
		return nil, err
	}
	model := schema.TableDesiredModel()
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(owner), Payload: payload})
	if err != nil {
		return nil, err
	}
	return value, nil
}

// FacetDiffers reports whether the table the observed facet describes does not
// already satisfy the desired facet. It applies the rules the alteration
// planner applies, so a difference reported here is one the planner can act on:
// the TTL, the automatic partitioning settings and the column families of a
// table facet, the family a column sits in for a column facet, and the names
// and definitions of the indexes for an index set. A changefeed set compares
// through ChangefeedDifference, which can also leave it undecided. Both
// envelopes must be validated and belong to one owner.
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
	case schema.IndexProperties:
		var want native.IndexSetIntent
		if err := protojson.Unmarshal(desired.Payload, &want); err != nil {
			return false, err
		}
		var held native.IndexObservations
		if err := protojson.Unmarshal(observed.Payload, &held); err != nil {
			return false, err
		}
		return indexDiffers(&want, &held), nil
	default:
		return false, fmt.Errorf("YDB facet comparison does not support %s", desired.GetKind())
	}
}

// tableDiffers applies the TTL, partitioning and family rules to the table properties. The
// declaration it reads manages the TTL, so one without a TTL differs from a
// table that holds one. The caller decides that the table is managed: either the
// declaration states properties, or the source established them.
func tableDiffers(want *native.RowTableIntent, held *native.DescribeTableResult) bool {
	if want.TtlSettings == nil && held.TtlSettings != nil || want.TtlSettings != nil && !sameTTL(want.TtlSettings, held.TtlSettings) {
		return true
	}
	if partitioningDelta(want.PartitioningSettings, held.PartitioningSettings) != nil {
		return true
	}
	// A layout the table was not created with differs, so that the plan reaches
	// the planner, which refuses it with the reason.
	if layoutRefusal(layoutOf(want), want.PartitioningSettings, held.PartitioningSettings) != "" {
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
