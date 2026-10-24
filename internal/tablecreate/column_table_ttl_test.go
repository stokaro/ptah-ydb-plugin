package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

const (
	// heldNoTTL is a column table hashed by id in three shards with no TTL.
	heldNoTTL = `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64"}`
	// deleteTwoDays deletes rows two days after ts.
	deleteTwoDays = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"172800","delete":{}}]}}`
)

// columnTTLRequest plans the ALTER of app.records from a held column storage
// to a declared one.
func columnTTLRequest(c *qt.C, declared, held string, capabilities map[string]bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := columnAlterRequest(c, []*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_DESIRED, declared)},
		[]*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, held)})
	for key, value := range capabilities {
		request.Capabilities[key] = value
	}
	return request
}

// TestColumnTablePlan_TTLChange_HappyPath changes a column table's TTL in
// place, as the host's embedded engine plans it: a RESET of the TTL the table
// holds, placed early, then a SET of the declared one. A table without a TTL
// takes the SET alone, and a declaration without one the RESET alone.
func TestColumnTablePlan_TTLChange_HappyPath(t *testing.T) {
	const heldDaily = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}`
	for _, test := range []struct {
		name, declared, held string
		capabilities         map[string]bool
		steps, payloads      []string
		placements           []protocol.PlanningMetadata_Placement
		// actions are what each step does to the table's TTL setting.
		actions      []protocol.PlanningEffect_Action
		dependencies int
	}{
		{"another interval", `{"ttl":` + deleteTwoDays + `}`, heldStorage, nil,
			[]string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"}, []string{`{"held":` + heldDaily + `}`, `{"set":` + deleteTwoDays + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY, protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER}, 1},
		{"a TTL on a table without one", `{"ttl":` + deleteDaily + `}`, heldNoTTL, nil,
			[]string{"column-table-ttl/000000/set"}, []string{`{"set":` + deleteDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_CREATE}, 0},
		{"no TTL", `{}`, heldStorage, nil,
			[]string{"column-table-ttl/000000/reset"}, []string{`{"held":` + heldDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_DROP}, 0},
		{"a disabled TTL", `{"ttl":{"disabled":{}}}`, heldStorage, nil,
			[]string{"column-table-ttl/000000/reset"}, []string{`{"held":` + heldDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_DROP}, 0},
		{"a tiered TTL on a target with tiering", `{"ttl":` + tiered + `}`, heldStorage, map[string]bool{tablecreate.TieredTTLCapability: true},
			[]string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"}, []string{`{"held":` + heldDaily + `}`, `{"set":` + tiered + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY, protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnTTLRequest(c, test.declared, test.held, test.capabilities)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			var steps, payloads []string
			var placements []protocol.PlanningMetadata_Placement
			var actions []protocol.PlanningEffect_Action
			for _, step := range plan.Contributions[0].Steps {
				steps = append(steps, step.Id.Name)
				payloads = append(payloads, string(step.Operation.Value.Value.Payload))
				placements = append(placements, step.Metadata.Placement)
				c.Assert(step.Metadata.Effects, qt.HasLen, 2)
				c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.ttl")
				actions = append(actions, step.Metadata.Effects[1].Action)
				c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.ColumnTTLOperationKind)
				c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			}
			c.Assert(steps, qt.DeepEquals, test.steps)
			c.Assert(placements, qt.DeepEquals, test.placements)
			c.Assert(actions, qt.DeepEquals, test.actions)
			c.Assert(len(payloads), qt.Equals, len(test.payloads))
			for i, payload := range payloads {
				c.Assert(jsonValue(c, payload), qt.DeepEquals, jsonValue(c, test.payloads[i]))
			}
			c.Assert(plan.Contributions[0].Dependencies, qt.HasLen, test.dependencies)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, len(test.steps))
			c.Assert(plan.Parents[0].Receipt.Strategy, qt.Matches, `Reset the column table's TTL and set the declared one .*`)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

// TestColumnTablePlan_TTLChangeAccountsForTheComparison carries out the change
// the comparison reports for a column table's TTL with the steps of its
// alteration.
func TestColumnTablePlan_TTLChangeAccountsForTheComparison(t *testing.T) {
	c := qt.New(t)
	request := columnTTLRequest(c, `{"ttl":`+deleteTwoDays+`}`, heldStorage, nil)
	table := request.Tables[0]
	comparison := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: proto.CloneOf(request.Identifiers), Desired: proto.CloneOf(table.Desired.Features.Facets), Current: proto.CloneOf(table.Current.Features.Facets),
		Parents: []*protocol.ComparisonParent{{Subject: proto.CloneOf(table.Subject), Desired: true, Current: true}}}
	compared, err := backend.New().CompareModels(t.Context(), comparison)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	c.Assert(compared.Changes[0].Value.Value.Kind, qt.Equals, schema.ColumnStorageChange)
	request.Changes = []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}
	plan, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Kind, qt.Equals, schema.ColumnStorageChange)
	c.Assert(plan.Changes[0].Strategy, qt.Matches, `The column table alteration resets the TTL .*`)
	c.Assert(plan.Changes[0].Steps, qt.HasLen, 2)
	c.Assert(plan.Changes[0].Steps[0].Name, qt.Equals, "column-table-ttl/000000/reset")
	c.Assert(plan.Changes[0].Steps[1].Name, qt.Equals, "column-table-ttl/000000/set")
}

// TestColumnTablePlan_TTLChange_FailurePath refuses a TTL the table cannot
// take, with the reason, and returns no partial plan.
func TestColumnTablePlan_TTLChange_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, declared, want string }{
		{"a tiered TTL on a target without tiering", `{"ttl":` + tiered + `}`,
			`table "app/records" declares a TTL tier that moves rows to an external data source, which requires target capability tiered_ttl, unavailable on this ydb target .*`},
		{"a TTL on a later key column", `{"ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			`TTL column "id" must be first in the primary key or have a LOCAL min_max index`},
		{"a TTL on an undeclared column", `{"ttl":{"enabled":{"column":"gone","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			`the TTL reads column "gone", which the table does not declare`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(t.Context(), columnTTLRequest(c, test.declared, heldStorage, nil))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestColumnTTLStatement(t *testing.T) {
	for _, test := range []struct{ name, payload, want string }{
		{"a reset", `{"held":` + deleteDaily + `}`, "ALTER TABLE `/local/app/records` RESET (TTL);"},
		{"a set", `{"set":` + tiered + `}`,
			"ALTER TABLE `/local/app/records` SET (TTL = Interval(\"PT86400S\") TO EXTERNAL DATA SOURCE `/local/ext`, Interval(\"PT604800S\") DELETE ON `ts`);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			model := tablecreate.ColumnTTLModel()
			alteration, err := tablecreate.ColumnTTLAlteration(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.ColumnTTLOperationKind, Owner: owner(),
				SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(test.payload)})
			c.Assert(err, qt.IsNil)
			c.Assert(tablecreate.ColumnTTLStatement("/local/app/records", alteration), qt.Equals, test.want)
		})
	}
}

// TestColumnTTLOperation_FailurePath refuses a TTL step that changes nothing
// or states what no declaration can.
func TestColumnTTLOperation_FailurePath(t *testing.T) {
	model := tablecreate.ColumnTTLModel()
	for _, test := range []struct{ name, payload, want string }{
		{"no TTL on either side", `{}`, `a column table TTL step changes the TTL`},
		{"the same TTL on both sides", `{"held":` + deleteDaily + `,"set":` + deleteDaily + `}`, `a column table TTL step changes the TTL`},
		{"a disabled TTL", `{"set":{"disabled":{}}}`, `a column table TTL step states a TTL or none`},
		{"a derived expiration", `{"set":{"enabled":{"column":"ts","expireAfterSeconds":"86400","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`,
			`a declared YDB column TTL states tiers; the server derives its expiration from them`},
		{"a delete before the last tier", `{"set":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"60","delete":{}},{"applyAfterSeconds":"120","externalStorage":"/local/ext"}]}}}`, `.+`},
		{"a null setting", `{"set":null}`, `YDB column table TTL step settings cannot be null`},
		{"an unknown field", `{"set":` + deleteDaily + `,"mode":"x"}`, `invalid YDB column table TTL step JSON`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			normalized, err := tablecreate.NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.ColumnTTLOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: model.Encoding,
				DefinitionSha256: model.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
