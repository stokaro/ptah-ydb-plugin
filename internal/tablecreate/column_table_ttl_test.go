package tablecreate_test

import (
	"maps"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	maps.Copy(request.Capabilities, capabilities)
	return request
}

// heldTiered is heldStorage with a TTL that moves rows to the data source ext
// after a day and deletes them after a week.
const heldTiered = `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":{"enabled":{"column":"ts","expireAfterSeconds":"604800",` +
	`"tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/ext"},{"applyAfterSeconds":"604800","delete":{}}]}}}`

// TestColumnTablePlan_TTLChange_HappyPath changes a column table's TTL in
// place, as the host's embedded engine plans it. A TTL that moves no row is
// replaced by a SET alone. A tiered TTL is reset first, placed early, and the
// declared one set after it, each step reading the data sources its TTL
// names. A table without a TTL takes the SET alone, and a declaration without
// one the RESET alone.
func TestColumnTablePlan_TTLChange_HappyPath(t *testing.T) {
	const (
		heldDaily      = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}`
		tieredTwoDays  = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"172800","externalStorage":"/local/ext"},{"applyAfterSeconds":"604800","delete":{}}]}}`
		tieringTarget  = true
		deletingTarget = false
	)
	for _, test := range []struct {
		name, declared, held string
		tiering              bool
		steps, payloads      []string
		placements           []protocol.PlanningMetadata_Placement
		// actions are what each step does to the table's TTL setting, and
		// reads the data sources each step reads.
		actions      []protocol.PlanningEffect_Action
		reads        []string
		dependencies int
	}{
		{"another interval", `{"ttl":` + deleteTwoDays + `}`, heldStorage, deletingTarget,
			[]string{"column-table-ttl/000000/set"}, []string{`{"held":` + heldDaily + `,"set":` + deleteTwoDays + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_DEFAULT}, []protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER}, []string{""}, 0},
		{"a TTL on a table without one", `{"ttl":` + deleteDaily + `}`, heldNoTTL, deletingTarget,
			[]string{"column-table-ttl/000000/set"}, []string{`{"set":` + deleteDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_CREATE}, []string{""}, 0},
		{"no TTL", `{}`, heldStorage, deletingTarget,
			[]string{"column-table-ttl/000000/reset"}, []string{`{"held":` + heldDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_DROP}, []string{""}, 0},
		{"a disabled TTL", `{"ttl":{"disabled":{}}}`, heldStorage, deletingTarget,
			[]string{"column-table-ttl/000000/reset"}, []string{`{"held":` + heldDaily + `}`}, []protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_DROP}, []string{""}, 0},
		{"a tiered TTL in place of one that deletes", `{"ttl":` + tiered + `}`, heldStorage, tieringTarget,
			[]string{"column-table-ttl/000000/set"}, []string{`{"held":` + heldDaily + `,"set":` + tiered + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_DEFAULT}, []protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER}, []string{"ext"}, 0},
		{"a TTL that deletes in place of a tiered one", `{"ttl":` + deleteDaily + `}`, heldTiered, tieringTarget,
			[]string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"}, []string{`{"held":` + tiered + `}`, `{"set":` + deleteDaily + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY, protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER}, []string{"ext", ""}, 1},
		{"another tiered TTL", `{"ttl":` + tieredTwoDays + `}`, heldTiered, tieringTarget,
			[]string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"}, []string{`{"held":` + tiered + `}`, `{"set":` + tieredTwoDays + `}`},
			[]protocol.PlanningMetadata_Placement{protocol.PlanningMetadata_EARLY, protocol.PlanningMetadata_DEFAULT},
			[]protocol.PlanningEffect_Action{protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER}, []string{"ext", "ext"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := columnTTLRequest(c, test.declared, test.held, map[string]bool{tablecreate.TieredTTLCapability: test.tiering})
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			var steps, payloads, reads []string
			var placements []protocol.PlanningMetadata_Placement
			var actions []protocol.PlanningEffect_Action
			for _, step := range plan.Contributions[0].Steps {
				steps = append(steps, step.Id.Name)
				payloads = append(payloads, string(step.Operation.Value.Value.Payload))
				placements = append(placements, step.Metadata.Placement)
				c.Assert(step.Metadata.Effects[0].Subject.Kind, qt.Equals, "table")
				c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.ttl")
				actions = append(actions, step.Metadata.Effects[1].Action)
				reads = append(reads, sourceReads(step))
				c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.ColumnTTLOperationKind)
				c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			}
			c.Assert(steps, qt.DeepEquals, test.steps)
			c.Assert(placements, qt.DeepEquals, test.placements)
			c.Assert(actions, qt.DeepEquals, test.actions)
			c.Assert(reads, qt.DeepEquals, test.reads)
			c.Assert(payloads, qt.HasLen, len(test.payloads))
			for i, payload := range payloads {
				c.Assert(jsonValue(c, payload), qt.DeepEquals, jsonValue(c, test.payloads[i]))
			}
			c.Assert(plan.Contributions[0].Dependencies, qt.HasLen, test.dependencies)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, len(test.steps))
			c.Assert(plan.Parents[0].Receipt.Strategy, qt.Matches, `Drop and add the column table's local indexes, and set its declared TTL in place of the one it holds, .*`)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

// sourceReads names the external data sources a step reads after the table
// and its TTL setting, joined by commas.
func sourceReads(step *protocol.PlanningStep) string {
	var names []string
	for _, effect := range step.GetMetadata().GetEffects()[2:] {
		if effect.GetSubject().GetKind() == schema.ExternalSourceProperties && effect.GetAction() == protocol.PlanningEffect_READ {
			names = append(names, path.Join(effect.GetSubject().GetSchema().GetNormalized(), effect.GetSubject().GetName().GetNormalized()))
		}
	}
	return strings.Join(names, ",")
}

// TestColumnTablePlan_TTLChangeAccountsForTheComparison carries out the change
// the comparison reports for a column table's TTL with the steps of its
// alteration.
func TestColumnTablePlan_TTLChangeAccountsForTheComparison(t *testing.T) {
	c := testcheck.New(t)
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
	c.Assert(plan.Changes[0].Strategy, qt.Matches, `The column table alteration sets the declared TTL in place of the one the table holds .*`)
	c.Assert(plan.Changes[0].Steps, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Steps[0].Name, qt.Equals, "column-table-ttl/000000/set")
}

// TestColumnTablePlan_TTLChange_FailurePath refuses a TTL the table cannot
// take, with the reason, and returns no partial plan.
func TestColumnTablePlan_TTLChange_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, declared, want string }{
		{"a tiered TTL on a target without tiering", `{"ttl":` + tiered + `}`,
			`table "app/records" declares a TTL tier that moves rows to an external data source, which requires target capability tiered_ttl, unavailable on this ydb target .*`},
		{"a TTL on a later key column", `{"ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			`TTL column "id" must be first in the primary key: YDB refuses one later in the key whatever local index reads it .*`},
		{"a TTL on an undeclared column", `{"ttl":{"enabled":{"column":"gone","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			`the TTL reads column "gone", which the table does not declare`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
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
			c := testcheck.New(t)
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
			c := testcheck.New(t)
			normalized, err := tablecreate.NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.ColumnTTLOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: model.Encoding,
				DefinitionSha256: model.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
