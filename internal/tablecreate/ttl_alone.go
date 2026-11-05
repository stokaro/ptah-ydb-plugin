package tablecreate

import (
	"bytes"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TTLAloneStorage returns the column storage a desired row-table record asks
// of a column table: the record states a TTL and nothing else, and the desired
// coverage does not establish the table's column storage, as an HCL document,
// which spells a TTL in a table's platform "ydb" block and no storage, does
// not. It is a declared column storage that holds that TTL as one deleting
// tier and leaves the hash key, the shard count and the local indexes to what
// the table holds. It is nil for any other record. The comparison and the
// alteration planner read a TTL alone through this one function, so a change
// the comparison reports is one the planner makes.
func TTLAloneStorage(coverage *protocol.ComparisonCoverage, record *protocol.ComparisonRecord) (*protocol.NormalizedValue, error) {
	if record.GetKind() != schema.TableProperties || record.GetValue() == nil || FacetKnown(coverage, schema.ColumnStorageProperties, record.GetSubject(), true) {
		return nil, nil
	}
	envelope, err := normalizedFacet(record, protocol.ValueInput_DESIRED)
	if err != nil {
		return nil, err
	}
	var intent native.RowTableIntent
	if err := protojson.Unmarshal(envelope.GetPayload(), &intent); err != nil {
		return nil, err
	}
	ttl := intent.GetTtlSettings()
	intent.TtlSettings, intent.PrimaryKey = nil, nil
	if ttl == nil || !proto.Equal(&intent, &native.RowTableIntent{}) {
		return nil, nil
	}
	policy, err := DeletingColumnTTL(ttl)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(&native.ColumnStorageIntent{Ttl: policy})
	if err != nil {
		return nil, err
	}
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.GetKind(), SchemaVersion: model.GetSchemaVersion(),
		Encoding: model.GetEncoding(), DefinitionSha256: model.GetDefinitionSha256(), Owner: proto.CloneOf(record.GetValue().GetValue().GetOwner()), Payload: payload})
	if err != nil {
		return nil, err
	}
	return &protocol.NormalizedValue{Id: record.GetValue().GetId(), Value: value, NormalizedOwner: proto.CloneOf(value.GetOwner()),
		CanonicalFormat: value.GetKind() + "/desired-json-v1", CanonicalProjection: bytes.Clone(value.GetPayload())}, nil
}

// columnTableTTL returns table with its declared row-table facet replaced by
// the column storage TTLAloneStorage reads it as, when the read holds the
// table as a column table. Every other table is returned as it is. The
// alteration and the rebuild of a column table read the replaced declaration,
// in which the TTL is the column table's.
func columnTableTTL(table *protocol.PlanningTable) (*protocol.PlanningTable, error) {
	held := false
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		held = held || record.GetKind() == schema.ColumnStorageProperties && record.GetValue() != nil
	}
	if !held {
		return table, nil
	}
	facets := table.GetDesired().GetFeatures().GetFacets()
	for i, record := range facets.GetRecords() {
		storage, err := TTLAloneStorage(facets.GetCoverage(), record)
		if err != nil {
			return nil, err
		}
		if storage == nil {
			continue
		}
		replaced := proto.CloneOf(table)
		replaced.Desired.Features.Facets.Records[i] = &protocol.ComparisonRecord{Subject: proto.CloneOf(record.GetSubject()), Kind: schema.ColumnStorageProperties,
			Targets: record.GetTargets(), Value: storage}
		return replaced, nil
	}
	return table, nil
}
