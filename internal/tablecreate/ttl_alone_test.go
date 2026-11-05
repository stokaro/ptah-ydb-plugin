package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// rowTTLFacet is a declared row-table facet of app.records that states the
// TTL alone, as an HCL platform block does.
func rowTTLFacet(c *qt.C, payload string) *protocol.ComparisonRecord {
	c.Helper()
	model := schema.TableDesiredModel()
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "table", Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind,
			SchemaVersion: model.SchemaVersion, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner(), Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.TableProperties, Subject: tableSubject(), Value: values.Values[0]}
}

// TestColumnTablePlan_TTLAlone plans the TTL alone a declaration that cannot
// say a table's storage states for a column table as the column table's TTL:
// the RESET and the SET a declared column storage with that TTL takes, for
// the change the comparison reports. The table keeps its storage, where the
// declaration would otherwise ask for a row table.
func TestColumnTablePlan_TTLAlone(t *testing.T) {
	c := testcheck.New(t)
	request := columnAlterRequest(c, []*protocol.ComparisonRecord{rowTTLFacet(c, `{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":172800}}}`)},
		[]*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, heldStorage)})
	table := request.Tables[0]
	comparison := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: proto.CloneOf(request.Identifiers), Desired: proto.CloneOf(table.Desired.Features.Facets), Current: proto.CloneOf(table.Current.Features.Facets),
		Parents: []*protocol.ComparisonParent{{Subject: proto.CloneOf(table.Subject), Desired: true, Current: true}}}
	compared, err := backend.New().CompareModels(t.Context(), comparison)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	request.Changes = []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}
	original := proto.CloneOf(request)

	plan, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Kind, qt.Equals, schema.ColumnStorageChange)
	c.Assert(plan.Changes[0].Steps, qt.HasLen, 2)
	c.Assert(plan.Changes[0].Steps[0].Name, qt.Equals, "column-table-ttl/000000/reset")
	c.Assert(plan.Changes[0].Steps[1].Name, qt.Equals, "column-table-ttl/000000/set")
	c.Assert(jsonValue(c, string(plan.Contributions[0].Steps[1].Operation.Value.Value.Payload)), qt.DeepEquals, jsonValue(c, `{"set":`+deleteTwoDays+`}`))
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

// A row-table facet that states more than a TTL asks for a row table, which a
// column table cannot become in place.
func TestColumnTablePlan_TTLAloneOnly(t *testing.T) {
	c := testcheck.New(t)
	request := columnAlterRequest(c, []*protocol.ComparisonRecord{rowTTLFacet(c, `{"keyBloomFilter":"ENABLED","ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":172800}}}`)},
		[]*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, heldStorage)})

	plan, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `table "app/records": changing between row and column storage requires an explicit data migration`)
}
