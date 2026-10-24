package tablecreate_test

import (
	"encoding/json"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func quoted(values ...string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// indexJSON states one index. kind is the native field of its type, such as
// globalIndex; cover may be empty.
func indexJSON(kind, name string, keys, cover []string) string {
	text := `"name":"` + name + `","indexColumns":` + quoted(keys...) + `,"` + kind + `":{}`
	if len(cover) > 0 {
		text += `,"dataColumns":` + quoted(cover...)
	}
	return text
}

// declaredIndexes is an index set a declaration states. Every declaration
// carries the requirement on the implementation table the model asks for; the
// planner reads only the specification.
func declaredIndexes(specs ...string) string {
	entries := make([]string, len(specs))
	for i, spec := range specs {
		entries[i] = `{"specification":{` + spec + `},"implementation":[{"name":"indexImplTable","settings":{"primaryKey":["label","id"]},` +
			`"columns":[{"name":"label","type":{"typeId":"UTF8"},"settings":{}},{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}]}`
	}
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

// heldIndexes is the index set a table holds, every index ready.
func heldIndexes(descriptions ...string) string {
	entries := make([]string, len(descriptions))
	for i, description := range descriptions {
		entries[i] = `{"description":{` + description + `,"status":"STATUS_READY"},"tables":[{"name":"indexImplTable","description":{` +
			`"columns":[{"name":"label","type":{"typeId":"UTF8"}},{"name":"id","type":{"typeId":"UINT64"}}],"primaryKey":["label","id"]}}]}`
	}
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

const vectorKind = `"globalVectorKmeansTreeIndex":{"vectorSettings":{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"clusters":2,"levels":1}}`

// vectorIndexes is an index set of one vector index, declared or held. A vector
// index is implemented by two tables.
func vectorIndexes(held bool) string {
	columns := `"columns":[{"name":"label","type":{"typeId":"UTF8"},"settings":{}},{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]`
	if held {
		observed := `"columns":[{"name":"label","type":{"typeId":"UTF8"}},{"name":"id","type":{"typeId":"UINT64"}}],"primaryKey":["label","id"]`
		return `{"indexes":[{"description":{"name":"by_embedding","indexColumns":["label"],` + vectorKind + `,"status":"STATUS_READY"},"tables":[` +
			`{"name":"indexImplLevelTable","description":{` + observed + `}},{"name":"indexImplPostingTable","description":{` + observed + `}}]}]}`
	}
	return `{"indexes":[{"specification":{"name":"by_embedding","indexColumns":["label"],` + vectorKind + `},"implementation":[` +
		`{"name":"indexImplLevelTable","settings":{"primaryKey":["label","id"]},` + columns + `},` +
		`{"name":"indexImplPostingTable","settings":{"primaryKey":["label","id"]},` + columns + `}]}]}`
}

// settings is a global index with the partitioning of its table, as the server
// reports it for a held index and as a declaration may state it.
const settings = `"name":"by_label","indexColumns":["label"],"globalIndex":{"settings":{"partitioningSettings":{"minPartitionsCount":"4"}}}`

func indexCase(desired, held string, declared ...string) alterCase {
	return alterCase{desiredTable: plainTable, heldTable: plainTable, declared: declared, held: declared, desiredIndexes: desired, heldIndexes: held}
}

var (
	byLabel      = []string{"label"}
	extra        = []string{"extra"}
	labelAndData = []string{"label", "extra"}
)

func TestIndexPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name       string
		in         alterCase
		want       string
		step       string
		impact     protocol.ChangeSemantics_Impact
		action     protocol.PlanningEffect_Action
		heldIndex  string
		indexOwner string
	}{
		{"adds a sync index", indexCase(declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes(), "label"),
			`{"addIndexes":[{"name":"by_label","indexColumns":["label"],"globalIndex":{}}]}`,
			"row-table-add-index/000000", protocol.ChangeSemantics_ADDITIVE, protocol.PlanningEffect_CREATE, "", "by_label"},
		{"adds an async index that covers a column", indexCase(declaredIndexes(indexJSON("globalAsyncIndex", "by_label", byLabel, extra)), heldIndexes(), "label", "extra"),
			`{"addIndexes":[{"name":"by_label","indexColumns":["label"],"dataColumns":["extra"],"globalAsyncIndex":{}}]}`,
			"row-table-add-index/000000", protocol.ChangeSemantics_ADDITIVE, protocol.PlanningEffect_CREATE, "", "by_label"},
		{"adds a unique index on two columns", indexCase(declaredIndexes(indexJSON("globalUniqueIndex", "by_both", labelAndData, nil)), heldIndexes(), "label", "extra"),
			`{"addIndexes":[{"name":"by_both","indexColumns":["label","extra"],"globalUniqueIndex":{}}]}`,
			"row-table-add-index/000000", protocol.ChangeSemantics_ADDITIVE, protocol.PlanningEffect_CREATE, "", "by_both"},
		{"adds one index beside a kept one", indexCase(
			declaredIndexes(indexJSON("globalIndex", "by_extra", extra, nil), indexJSON("globalIndex", "by_label", byLabel, nil)),
			heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label", "extra"),
			`{"addIndexes":[{"name":"by_extra","indexColumns":["extra"],"globalIndex":{}}]}`,
			"row-table-add-index/000000", protocol.ChangeSemantics_ADDITIVE, protocol.PlanningEffect_CREATE, "", "by_extra"},
		{"drops an index the declaration leaves out", indexCase(declaredIndexes(), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, extra)), "label", "extra"),
			`{"dropIndexes":["by_label"]}`,
			"row-table-drop-index/000000", protocol.ChangeSemantics_DESTRUCTIVE, protocol.PlanningEffect_DROP, indexJSON("globalIndex", "by_label", byLabel, extra), "by_label"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, test.in)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, test.step)
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.AlterOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, test.impact)
			c.Assert(step.Operation.Value.OperationSemantics.Impact, qt.Equals, test.impact)
			// The step reads the table and creates or drops one index object.
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			c.Assert(step.Metadata.Effects[0].Subject.Kind, qt.Equals, "table")
			c.Assert(step.Metadata.Effects[1].Action, qt.Equals, test.action)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "index")
			c.Assert(step.Metadata.Effects[1].Subject.Name.Source, qt.Equals, test.indexOwner)
			c.Assert(step.Metadata.Effects[1].Subject.Parent.Source, qt.Equals, "records")
			c.Assert(plan.Parents, qt.HasLen, 5)
			for _, parent := range plan.Parents {
				c.Assert(parent.Receipt.Steps, qt.HasLen, 1)
				c.Assert(parent.Receipt.Steps[0].Name, qt.Equals, test.step)
			}
			alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			got, err := protojson.Marshal(alteration.Request)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.want))
			c.Assert(alteration.HeldIndexes, qt.HasLen, len(strings.Fields(test.heldIndex)))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func TestIndexPlan_TwoChangesAreTwoSteps(t *testing.T) {
	c := qt.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, indexCase(
		declaredIndexes(indexJSON("globalIndex", "by_extra", extra, nil)),
		heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label", "extra")))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	var names []string
	for _, step := range plan.Contributions[0].Steps {
		names = append(names, step.Id.Name)
	}
	// An addition and a removal are separate operations, in index name order.
	c.Assert(names, qt.DeepEquals, []string{"row-table-add-index/000000", "row-table-drop-index/000000"})
	c.Assert(edges(plan), qt.HasLen, 0)
}

func TestIndexPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"neither side has an index", indexCase(declaredIndexes(), heldIndexes(), "label")},
		{"the table holds the declared index", indexCase(
			declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, extra)), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, extra)), "label", "extra")},
		{"the sets are the same in another order", indexCase(
			declaredIndexes(indexJSON("globalIndex", "b", byLabel, nil), indexJSON("globalAsyncIndex", "a", extra, nil)),
			heldIndexes(indexJSON("globalAsyncIndex", "a", extra, nil), indexJSON("globalIndex", "b", byLabel, nil)), "label", "extra")},
		{"the partitioning the server reports for a held index is not compared", indexCase(
			declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes(settings), "label")},
		{"a declaration without an index set leaves the indexes alone", indexCase("", heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 5)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

func TestIndexPlan_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"a key column changed", indexCase(declaredIndexes(indexJSON("globalIndex", "by_label", extra, nil)), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label", "extra"),
			`index "by_label" holds a global index on \(label\), but the declaration states a global index on \(extra\); YDB cannot alter an index in place.*rebuilds the index`},
		{"a covered column changed", indexCase(declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, extra)), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label", "extra"),
			`index "by_label" holds a global index on \(label\), but the declaration states a global index on \(label\) covering \(extra\); YDB cannot alter.*`},
		{"the kind changed", indexCase(declaredIndexes(indexJSON("globalAsyncIndex", "by_label", byLabel, nil)), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), "label"),
			`index "by_label" holds a global index on \(label\), but the declaration states a global async index on \(label\); YDB cannot alter.*`},
		{"the key order changed", indexCase(declaredIndexes(indexJSON("globalIndex", "by_both", []string{"extra", "label"}, nil)), heldIndexes(indexJSON("globalIndex", "by_both", labelAndData, nil)), "label", "extra"),
			`index "by_both" holds a global index on \(label, extra\), but the declaration states a global index on \(extra, label\).*`},
		{"a declared index states settings", indexCase(declaredIndexes(settings), heldIndexes(), "label"),
			`the declared index "by_label" is outside the global secondary indexes this provider alters: it states settings of its own`},
		{"a vector index is declared", indexCase(vectorIndexes(false), heldIndexes(), "label"),
			`the declared index "by_embedding" is outside the global secondary indexes this provider alters: it is a vector index`},
		{"a vector index is left out", indexCase(declaredIndexes(), vectorIndexes(true), "label"),
			`the index "by_embedding" the declaration leaves out is outside the global secondary indexes this provider alters: it is a vector index`},
		{"a declared index reads an undeclared column", indexCase(declaredIndexes(indexJSON("globalIndex", "by_other", []string{"other"}, nil)), heldIndexes(), "label"),
			`index "by_other" reads column "other", which the declaration does not state`},
		{"a kept index reads a column the declaration drops", indexCase(
			declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))),
			`index "by_label" reads column "label", which the declaration does not state`},
		{"the table's indexes were not captured", alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label"},
			desiredIndexes: declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))},
			`The table's current indexes were not captured`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestIndexPlan_NoChangeForAnUnchangedVectorIndex(t *testing.T) {
	c := qt.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, indexCase(vectorIndexes(false), vectorIndexes(true), "label")))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}

func TestIndexPlan_OrdersAgainstColumnSteps(t *testing.T) {
	for _, test := range []struct {
		name  string
		in    alterCase
		steps []*protocol.CommonPlanningStep
		edges []string
	}{
		{"an addition follows the addition of the column the index reads",
			alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label", "other"}, held: nil,
				desiredIndexes: declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes: heldIndexes()},
			[]*protocol.CommonPlanningStep{additionStep("add-label", "label", nil), additionStep("add-other", "other", nil)},
			[]string{"row-table-add-column/000000 -> row-table-add-index/000000"}},
		{"a removal precedes the removal of the column the index reads",
			alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"other"}, held: []string{"label", "other"},
				desiredIndexes: declaredIndexes(), heldIndexes: heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))},
			[]*protocol.CommonPlanningStep{removalStep("drop-label", "label")},
			[]string{"row-table-drop-index/000000 -> row-table-drop-column/000000"}},
		{"a removal precedes the removal of a column it covers",
			alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label"}, held: []string{"label", "extra"},
				desiredIndexes: declaredIndexes(), heldIndexes: heldIndexes(indexJSON("globalIndex", "by_label", byLabel, extra))},
			[]*protocol.CommonPlanningStep{removalStep("drop-extra", "extra")},
			[]string{"row-table-drop-index/000000 -> row-table-drop-column/000000"}},
		{"a column step of another column is not ordered",
			alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label", "fresh"}, held: []string{"label", "old"},
				desiredIndexes: declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes: heldIndexes()},
			[]*protocol.CommonPlanningStep{additionStep("add-fresh", "fresh", nil), removalStep("drop-old", "old")},
			nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), columnRequest(c, test.in, test.steps...))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(edges(plan), qt.DeepEquals, test.edges)
		})
	}
}
