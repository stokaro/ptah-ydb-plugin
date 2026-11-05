package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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

// A global index the native read leaves to the host is dropped as the host's
// read of the table describes it.
func TestIndexClaims_DropAnIndexTheHostRead(t *testing.T) {
	c := testcheck.New(t)
	request := commonIndexRequest(c, heldIndexes(), nil, commonIndexStep("drop", "by_label", protocol.PlanningEffect_DROP))
	request.Tables[0].Current.Indexes = []*protocol.ObservedIndex{{Name: "by_label", TableName: "records", Schema: "app", Columns: []string{"label"}, IncludeColumns: []string{"extra"}, Method: "async"}}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"drop => row-table-drop-index/000000"})
	alteration, err := tablecreate.DecodeAlteration(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(alteration.HeldIndexes, qt.HasLen, 1)
	held, err := protojson.Marshal(alteration.HeldIndexes[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(held), qt.JSONEquals, jsonValue(c, `{"name":"by_label","indexColumns":["label"],"dataColumns":["extra"],"globalAsyncIndex":{}}`))
}

// commonRenameStep is the host's step that renames index from of
// app.records to to in place: it reads the table, drops from and creates to.
func commonRenameStep(id, from, to string) *protocol.CommonPlanningStep {
	step := commonStep(id, tableSubject(), "",
		&protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: indexSubject(from), Action: protocol.PlanningEffect_DROP},
		&protocol.PlanningEffect{Subject: indexSubject(to), Action: protocol.PlanningEffect_CREATE})
	step.RenamedIndex = &protocol.CommonIndexRename{From: from, To: to}
	return step
}

// renameRequest alters app.records, which holds old_label and declares it as
// by_label, on a host that lists planning.index-renames, with the host's
// rename step and the session's index_rename as rename states it.
func renameRequest(c *qt.C, rename bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := commonIndexRequest(c, heldIndexes(indexJSON("globalIndex", "old_label", byLabel, nil)),
		[]*protocol.DeclaredIndex{{Name: "by_label", Fields: []string{"label"}, IncludeColumns: []string{"extra"}}},
		commonRenameStep("rename", "old_label", "by_label"))
	request.TargetFeatures = append(request.TargetFeatures, provider.IndexRenamesFeature)
	request.Tables[0].Current.Indexes = []*protocol.ObservedIndex{{Name: "old_label", TableName: "records", Schema: "app", Columns: []string{"label"}, IncludeColumns: []string{"extra"}}}
	request.Capabilities[tablecreate.IndexRenameCapability] = rename
	return request
}

// The host's step that renames an index is replaced by one alteration that
// renames it, which keeps the index, and the step keeps the host's footprint.
func TestIndexClaims_RenameInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := renameRequest(c, true)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"rename => row-table-rename-index/000000"})
	step := plan.Contributions[0].Steps[0]
	c.Assert(alterationJSON(c, step), qt.JSONEquals, jsonValue(c, `{"renameIndexes":[{"sourceName":"old_label","destinationName":"by_label"}]}`))
	c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(step.Metadata.Effects, qt.HasLen, 3)
	for n, effect := range request.CommonSteps[0].Metadata.Effects {
		c.Assert(proto.Equal(step.Metadata.Effects[n], effect), qt.IsTrue)
	}
	alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(alteration.HeldIndexes, qt.HasLen, 1)
	c.Assert(alteration.HeldIndexes[0].Name, qt.Equals, "old_label")
	text, err := tablecreate.AlterationStatement("app/records", alteration)
	c.Assert(err, qt.IsNil)
	c.Assert(text, qt.Equals, "ALTER TABLE `app/records` RENAME INDEX `old_label` TO `by_label`;")
}

// A rename the cluster cannot make is refused before dispatch, in the words
// of the flag that decides it.
func TestIndexClaims_RenameInPlace_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), renameRequest(c, false))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `The host renames index "old_label" to "by_label", and this cluster renames no index in place: EnableMoveIndex is off`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
