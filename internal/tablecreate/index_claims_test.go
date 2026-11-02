package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func indexSubject(name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "index", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part(name)}
}

// commonIndexStep is a host step that creates or drops index name of
// app.records, with the footprint the host gives a common index change: a
// read of the table and the change of the index.
func commonIndexStep(id, name string, action protocol.PlanningEffect_Action) *protocol.CommonPlanningStep {
	return commonStep(id, tableSubject(), "",
		&protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: indexSubject(name), Action: action})
}

// commonIndexRequest alters app.records, which holds the indexes held, while
// the declaration states the common indexes declared.
func commonIndexRequest(c *qt.C, held string, declared []*protocol.DeclaredIndex, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label", "extra"}, held: []string{"label", "extra"},
		heldIndexes: held}, steps...)
	request.Tables[0].Desired.Indexes = declared
	return request
}

func alterationJSON(c *qt.C, step *protocol.PlanningStep) string {
	c.Helper()
	alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	encoded, err := protojson.Marshal(alteration.Request)
	c.Assert(err, qt.IsNil)
	return string(encoded)
}

// The host's step that adds a declared common index and its step that drops
// a held one are each replaced by an alteration of the table, which changes
// the index the host's step names.
func TestIndexClaims_ReplaceTheHostSteps(t *testing.T) {
	c := testcheck.New(t)
	request := commonIndexRequest(c, heldIndexes(indexJSON("globalIndex", "old_label", byLabel, nil)),
		[]*protocol.DeclaredIndex{{Name: "by_extra", Fields: []string{"extra"}, Type: "async", IncludeColumns: []string{"label"}}},
		commonIndexStep("add-by-extra", "by_extra", protocol.PlanningEffect_CREATE), commonIndexStep("drop-old", "old_label", protocol.PlanningEffect_DROP))
	original := proto.CloneOf(request)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"drop-old => row-table-drop-index/000000", "add-by-extra => row-table-add-index/000000"})
	steps := plan.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(alterationJSON(c, steps[0]), qt.JSONEquals, jsonValue(c, `{"dropIndexes":["old_label"]}`))
	c.Assert(alterationJSON(c, steps[1]), qt.JSONEquals, jsonValue(c, `{"addIndexes":[{"name":"by_extra","indexColumns":["extra"],"dataColumns":["label"],"globalAsyncIndex":{}}]}`))
	c.Assert(proto.Equal(steps[0].Metadata.Effects[1].Subject, indexSubject("old_label")), qt.IsTrue)
	c.Assert(steps[0].Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_DROP)
	c.Assert(proto.Equal(steps[1].Metadata.Effects[1].Subject, indexSubject("by_extra")), qt.IsTrue)
	c.Assert(steps[1].Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_CREATE)
	c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 2)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

// An index whose definition changes under its name is dropped before it is
// added again.
func TestIndexClaims_DropBeforeTheReplacement(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), commonIndexRequest(c, heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)),
		[]*protocol.DeclaredIndex{{Name: "by_label", Fields: []string{"label", "extra"}}},
		commonIndexStep("add-by-label", "by_label", protocol.PlanningEffect_CREATE), commonIndexStep("drop-by-label", "by_label", protocol.PlanningEffect_DROP)))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"drop-by-label => row-table-drop-index/000000", "add-by-label => row-table-add-index/000000"})
	c.Assert(edges(plan), qt.DeepEquals, []string{"row-table-drop-index/000000 -> row-table-add-index/000000"})
}

func TestIndexClaims_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		held     string
		declared []*protocol.DeclaredIndex
		steps    []*protocol.CommonPlanningStep
		message  string
	}{
		{"an addition the declaration does not state", heldIndexes(), nil,
			[]*protocol.CommonPlanningStep{commonIndexStep("add", "by_label", protocol.PlanningEffect_CREATE)},
			`The host adds index "by_label", which the declaration does not state`},
		{"a removal the table does not hold", heldIndexes(), nil,
			[]*protocol.CommonPlanningStep{commonIndexStep("drop", "by_label", protocol.PlanningEffect_DROP)},
			`The host drops index "by_label", which the table's index read does not hold`},
		{"a removal without the table's index read", "", nil,
			[]*protocol.CommonPlanningStep{commonIndexStep("drop", "by_label", protocol.PlanningEffect_DROP)},
			`The host drops index "by_label", which the table's index read does not hold`},
		{"an addition YDB cannot create", heldIndexes(), []*protocol.DeclaredIndex{{Name: "by_label", Fields: []string{"label"}, Type: "vector_kmeans_tree"}},
			[]*protocol.CommonPlanningStep{commonIndexStep("add", "by_label", protocol.PlanningEffect_CREATE)},
			`index "by_label": it is a vector_kmeans_tree index, and this provider creates a table with its global indexes.*`},
		{"the same addition twice", heldIndexes(), []*protocol.DeclaredIndex{{Name: "by_label", Fields: []string{"label"}}},
			[]*protocol.CommonPlanningStep{commonIndexStep("add", "by_label", protocol.PlanningEffect_CREATE), commonIndexStep("again", "by_label", protocol.PlanningEffect_CREATE)},
			`The host changes index "by_label" more than once`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), commonIndexRequest(c, test.held, test.declared, test.steps...))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// A table that states its index set natively and has the host drop one of
// its indexes as a common one changes that index twice.
func TestIndexClaims_RefuseAnIndexChangedTwice(t *testing.T) {
	c := testcheck.New(t)
	request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label"}, held: []string{"label"},
		desiredIndexes: declaredIndexes(), heldIndexes: heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))},
		commonIndexStep("drop", "by_label", protocol.PlanningEffect_DROP))
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `Index "by_label" is declared both in the native index set and as a common index`)
}
