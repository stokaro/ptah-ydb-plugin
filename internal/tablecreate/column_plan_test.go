package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func columnSubject(name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "column", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part(name)}
}

// additionStep is a host step that adds column name, with the footprint the host
// gives a column addition: a read of the table and the creation of the column.
func additionStep(id, name string, change func(*protocol.PlanningColumn)) *protocol.CommonPlanningStep {
	step := commonStep(id, tableSubject(), name,
		&protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: columnSubject(name), Action: protocol.PlanningEffect_CREATE})
	if change != nil {
		change(step.AddedColumn)
	}
	return step
}

// removalStep is a host step that drops column name.
func removalStep(id, name string) *protocol.CommonPlanningStep {
	return commonStep(id, tableSubject(), "",
		&protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: columnSubject(name), Action: protocol.PlanningEffect_DROP})
}

func edges(plan *protocol.PlannedFeatures) []string {
	var result []string
	for _, contribution := range plan.Contributions {
		for _, dependency := range contribution.Dependencies {
			result = append(result, dependency.Before.Name+" -> "+dependency.After.Name)
		}
	}
	return result
}

func rewrites(plan *protocol.PlannedFeatures) []string {
	var result []string
	for _, rewrite := range plan.Rewrites {
		result = append(result, rewrite.Sources[0].Name+" => "+rewrite.Replacement.Name)
	}
	return result
}

// columnRequest captures an ALTER of app.records that adds payload and other and
// drops old, with the given host steps.
func columnRequest(c *qt.C, in alterCase, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, in)
	request.CommonSteps = steps
	return request
}

func TestColumnPlan_ClaimsAdditionsAndRemovals(t *testing.T) {
	c := qt.New(t)
	request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload", "other"}, held: []string{"old", "older"}},
		additionStep("add-payload", "payload", nil), additionStep("add-other", "other", nil),
		removalStep("drop-old", "old"), removalStep("drop-older", "older"))
	original := proto.CloneOf(request)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{
		"add-payload => row-table-add-column/000000", "add-other => row-table-add-column/000001",
		"drop-old => row-table-drop-column/000000", "drop-older => row-table-drop-column/000001"})
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 4)
	c.Assert(plan.Contributions[0].Dependencies, qt.HasLen, 0)
	for _, step := range plan.Contributions[0].Steps {
		c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.AlterOperationKind)
		c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
	}
	first := plan.Contributions[0].Steps[0]
	c.Assert(first.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_ADDITIVE)
	c.Assert(first.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_CREATE)
	c.Assert(first.Metadata.Effects[1].Subject.Name.Source, qt.Equals, "payload")
	alteration, err := tablecreate.DecodeAlteration(first.Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	got, err := protojson.Marshal(alteration.Request)
	c.Assert(err, qt.IsNil)
	c.Assert(string(got), qt.JSONEquals, jsonValue(c, `{"addColumns":[{"name":"payload","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}]}`))
	last := plan.Contributions[0].Steps[2]
	c.Assert(last.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_DESTRUCTIVE)
	c.Assert(last.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_DROP)
	alteration, err = tablecreate.DecodeAlteration(last.Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(alteration.Request.DropColumns, qt.DeepEquals, []string{"old"})
	// Both parent kinds name every step, so none is left unaccounted for.
	c.Assert(plan.Parents, qt.HasLen, 4)
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 4)
	}
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

func TestColumnPlan_FamilyOperationFollowsAdditionsAndPrecedesRemovals(t *testing.T) {
	c := qt.New(t)
	request := columnRequest(c, alterCase{
		desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`},
		declared: []string{"payload", "other"}, heldTable: plainTable, held: []string{"old"}},
		additionStep("add-payload", "payload", nil), additionStep("add-other", "other", nil), removalStep("drop-old", "old"),
		// A step the native call cannot state stays with the host and keeps its own identity.
		additionStep("add-defaulted", "payload", func(column *protocol.PlanningColumn) {
			column.Default = &protocol.PlanningDefault{Value: "x", ValueSet: true}
		}))
	request.CommonSteps = request.CommonSteps[:3]
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 4)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{
		"add-payload => row-table-add-column/000000", "add-other => row-table-add-column/000001", "drop-old => row-table-drop-column/000000"})
	// Only the addition of the column the family operation moves is an edge, and
	// both ends are steps of this provider.
	c.Assert(edges(plan), qt.DeepEquals, []string{
		"row-table-add-column/000000 -> row-table-alter/000000", "row-table-alter/000000 -> row-table-drop-column/000000"})
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 4)
		var names []string
		for _, step := range parent.Receipt.Steps {
			names = append(names, step.Name)
		}
		c.Assert(names, qt.DeepEquals, []string{"row-table-add-column/000000", "row-table-add-column/000001", "row-table-alter/000000", "row-table-drop-column/000000"})
	}
}

func TestColumnPlan_LeavesWhatTheCallCannotState(t *testing.T) {
	for _, test := range []struct {
		name string
		step *protocol.CommonPlanningStep
	}{
		{"not null", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Nullable = false })},
		{"default value", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Default = &protocol.PlanningDefault{Value: "x", ValueSet: true} })},
		{"default expression", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Default = &protocol.PlanningDefault{Expression: "1"} })},
		{"key column", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Primary = true })},
		{"unique", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Unique = true })},
		{"check", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Check = "payload <> ''" })},
		{"foreign key", additionStep("add", "payload", func(c *protocol.PlanningColumn) {
			c.ForeignKey = &protocol.PlanningForeignKey{Table: "other", Column: "id"}
		})},
		{"generated", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.GeneratedExpression = "1" })},
		{"comment", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Comment = "note" })},
		{"unknown type", additionStep("add", "payload", func(c *protocol.PlanningColumn) { c.Type = "Struct<a:Int32>" })},
		{"other footprint", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_ALTER},
			&protocol.PlanningEffect{Subject: columnSubject("payload"), Action: protocol.PlanningEffect_CREATE})},
		{"two columns", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: columnSubject("payload"), Action: protocol.PlanningEffect_CREATE},
			&protocol.PlanningEffect{Subject: columnSubject("other"), Action: protocol.PlanningEffect_CREATE})},
		{"no footprint", commonStep("add", tableSubject(), "payload")},
		{"reads another object", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: columnSubject("other"), Action: protocol.PlanningEffect_READ},
			&protocol.PlanningEffect{Subject: columnSubject("payload"), Action: protocol.PlanningEffect_CREATE})},
		{"removal of an index", commonStep("drop", tableSubject(), "",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "index", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part("payload")}, Action: protocol.PlanningEffect_DROP})},
		{"creation of an index named like the column", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "index", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part("payload")}, Action: protocol.PlanningEffect_CREATE})},
		{"column of another database", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "column", Database: part("/other"), Schema: part("app"), Parent: part("records"), Name: part("payload")}, Action: protocol.PlanningEffect_CREATE})},
		{"column of another directory", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "column", Database: part("/local"), Schema: part("elsewhere"), Parent: part("records"), Name: part("payload")}, Action: protocol.PlanningEffect_CREATE})},
		{"folded spelling", commonStep("add", tableSubject(), "Payload",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "column", Database: part("/local"), Schema: part("app"), Parent: part("records"),
				Name: &protocol.ComparisonPart{Source: "Payload", Normalized: "payload"}}, Action: protocol.PlanningEffect_CREATE})},
		{"addition that drops", func() *protocol.CommonPlanningStep {
			step := additionStep("add", "payload", nil)
			step.Metadata.Effects[1].Action = protocol.PlanningEffect_DROP
			return step
		}()},
		{"column name with a separator", func() *protocol.CommonPlanningStep {
			step := commonStep("add", tableSubject(), "pay/load",
				&protocol.PlanningEffect{Subject: columnSubject("pay/load"), Action: protocol.PlanningEffect_CREATE})
			return step
		}()},
		{"column of another table", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{Kind: "column", Database: part("/local"), Schema: part("app"), Parent: part("elsewhere"), Name: part("payload")}, Action: protocol.PlanningEffect_CREATE})},
		{"another name", commonStep("add", tableSubject(), "payload",
			&protocol.PlanningEffect{Subject: columnSubject("renamed"), Action: protocol.PlanningEffect_CREATE})},
		{"alteration of a column", commonStep("alter", tableSubject(), "",
			&protocol.PlanningEffect{Subject: columnSubject("payload"), Action: protocol.PlanningEffect_ALTER})},
		{"removal that also creates", commonStep("drop", tableSubject(), "",
			&protocol.PlanningEffect{Subject: columnSubject("payload"), Action: protocol.PlanningEffect_CREATE})},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}}, test.step)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
		})
	}
}

func TestColumnPlan_ClaimsOnlyStepsOfItsOwnTable(t *testing.T) {
	c := qt.New(t)
	request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}})
	second := alterRequest(c, alterCase{declared: []string{"payload"}})
	accounts := &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("accounts")}
	second.Tables[0].Subject = accounts
	second.Tables[0].Desired.Name = "accounts"
	second.Tables[0].Current.Name = "accounts"
	request.Tables = append(request.Tables, second.Tables[0])
	// The step is bound to accounts, but its footprint names a column of records.
	step := additionStep("add", "payload", nil)
	step.Parent = accounts
	request.CommonSteps = []*protocol.CommonPlanningStep{step}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Rewrites, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}

func TestColumnPlan_KeepsPlacement(t *testing.T) {
	c := qt.New(t)
	step := additionStep("add", "payload", nil)
	step.Metadata.Placement = protocol.PlanningMetadata_EARLY
	plan, err := backend.New().PlanFeatures(c.Context(), columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}}, step))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions[0].Steps[0].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_EARLY)
}

func TestColumnPlan_ClaimsWhatTheCallCanState(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*protocol.PlanningColumn)
	}{
		{"nullable column", nil},
		{"declared type text", func(c *protocol.PlanningColumn) { c.TypeIsDeclaredText = true }},
		{"decimal", func(c *protocol.PlanningColumn) { c.Type = "Decimal(22,9)" }},
		{"empty default", func(c *protocol.PlanningColumn) { c.Default = &protocol.PlanningDefault{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}}, additionStep("add", "payload", test.change))
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(rewrites(plan), qt.DeepEquals, []string{"add => row-table-add-column/000000"})
		})
	}
}

func TestColumnPlan_AddedColumnFacets(t *testing.T) {
	captured := func(c *qt.C, name, payload string) *protocol.CapturedFacet {
		c.Helper()
		record := facet(c, schema.ColumnProperties, name, payload)
		return &protocol.CapturedFacet{Kind: record.Kind, Value: record.Value}
	}
	for _, test := range []struct {
		name    string
		facet   func(*qt.C) *protocol.CapturedFacet
		claimed bool
	}{
		{"family only", func(c *qt.C) *protocol.CapturedFacet { return captured(c, "payload", `{"family":"archive"}`) }, true},
		{"empty properties", func(c *qt.C) *protocol.CapturedFacet { return captured(c, "payload", `{}`) }, true},
		{"literal default", func(c *qt.C) *protocol.CapturedFacet {
			return captured(c, "payload", `{"literal":{"type":{"typeId":"UTF8"},"value":{"textValue":"x"}}}`)
		}, false},
		{"another target", func(c *qt.C) *protocol.CapturedFacet {
			result := captured(c, "payload", `{"literal":{"type":{"typeId":"UTF8"},"value":{"textValue":"x"}}}`)
			result.Targets = []string{"postgres"}
			return result
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}},
				additionStep("add", "payload", func(column *protocol.PlanningColumn) { column.Facets = []*protocol.CapturedFacet{test.facet(c)} }))
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(len(plan.Rewrites) == 1, qt.Equals, test.claimed)
		})
	}
}

func TestColumnPlan_RefusesForgedAddedColumnFacets(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*qt.C, *protocol.CapturedFacet)
	}{
		{"projection", func(c *qt.C, f *protocol.CapturedFacet) { f.Value.CanonicalProjection = []byte(`{"family":"cold"}`) }},
		{"other database", func(c *qt.C, f *protocol.CapturedFacet) {
			f.Value.Value.Owner.Database = "/other"
			f.Value.NormalizedOwner.Database = "/other"
		}},
		{"other directory", func(c *qt.C, f *protocol.CapturedFacet) {
			f.Value.Value.Owner.Schema = "elsewhere"
			f.Value.NormalizedOwner.Schema = "elsewhere"
		}},
		{"outer kind", func(_ *qt.C, f *protocol.CapturedFacet) { f.Kind = schema.TableProperties }},
		{"table properties", func(c *qt.C, f *protocol.CapturedFacet) {
			table := facet(c, schema.TableProperties, "", plainTable)
			f.Kind, f.Value = table.Kind, table.Value
		}},
		{"other column", func(c *qt.C, f *protocol.CapturedFacet) {
			f.Value.Value.Owner.Name = "elsewhere"
			f.Value.NormalizedOwner.Name = "elsewhere"
		}},
		{"other table", func(c *qt.C, f *protocol.CapturedFacet) {
			f.Value.Value.Owner.Path = []string{"elsewhere"}
			f.Value.NormalizedOwner.Path = []string{"elsewhere"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			record := facet(c, schema.ColumnProperties, "payload", `{"family":"archive"}`)
			captured := &protocol.CapturedFacet{Kind: record.Kind, Value: proto.CloneOf(record.Value)}
			test.modify(c, captured)
			request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}},
				additionStep("add", "payload", func(column *protocol.PlanningColumn) { column.Facets = []*protocol.CapturedFacet{captured} }))
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(plan, qt.IsNil)
		})
	}
}

func TestColumnPlan_RefusesWhatTheDeclarationContradicts(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		steps   []*protocol.CommonPlanningStep
		message string
	}{
		{"addition of an undeclared column", alterCase{desiredTable: plainTable, heldTable: plainTable},
			[]*protocol.CommonPlanningStep{additionStep("add", "ghost", nil)}, `The host adds column "ghost", which the declaration does not state`},
		{"removal of a declared column", alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}, held: []string{"payload"}},
			[]*protocol.CommonPlanningStep{removalStep("drop", "payload")}, `The host drops column "payload", which the declaration states`},
		{"column changed twice", alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}, held: []string{"old"}},
			[]*protocol.CommonPlanningStep{additionStep("add", "payload", nil), additionStep("again", "payload", nil)}, `The host changes column "payload" more than once`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), columnRequest(c, test.in, test.steps...))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 0)
		})
	}
}

func TestColumnPlan_NumbersStepsAcrossTables(t *testing.T) {
	c := qt.New(t)
	request := columnRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"payload"}}, additionStep("add-a", "payload", nil))
	second := alterRequest(c, alterCase{declared: []string{"payload"}})
	accounts := &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("accounts")}
	second.Tables[0].Subject = accounts
	second.Tables[0].Desired.Name = "accounts"
	second.Tables[0].Current.Name = "accounts"
	request.Tables = append(request.Tables, second.Tables[0])
	add := additionStep("add-b", "payload", nil)
	add.Parent = accounts
	add.Metadata.Effects[0].Subject = accounts
	add.Metadata.Effects[1].Subject.Parent = part("accounts")
	request.CommonSteps = append(request.CommonSteps, add)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	// Each table claims its own step, and identities stay unique across them.
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"add-b => row-table-add-column/000000", "add-a => row-table-add-column/000001"})
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 2)
	c.Assert(plan.Contributions[0].Steps[0].Metadata.Effects[1].Subject.Parent.Source, qt.Equals, "accounts")
}

func TestColumnOperation_FailurePath(t *testing.T) {
	for _, payload := range []string{
		// A request does one thing.
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],"dropColumns":["b"]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],"addColumnFamilies":[{"name":"f"}]}}`,
		`{"request":{"dropColumns":["a"],"addColumnFamilies":[{"name":"f"}]}}`,
		// Only nullable columns without a default or a family are added.
		`{"request":{"addColumns":[{"name":"a","type":{"typeId":"UTF8"},"notNull":true}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"typeId":"UTF8"}}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"typeId":"UTF8"},"notNull":false}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}}}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":true}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false,"family":"f"}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false,"fromLiteral":{"type":{"typeId":"UTF8"},"value":{"textValue":"x"}}}]}}`,
		`{"request":{"addColumns":[{"name":"a","notNull":false}]}}`,
		`{"request":{"addColumns":[{"name":"a/b","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false},{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}]}}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"listType":{"item":{"typeId":"UTF8"}}}}},"notNull":false}]}}`,
		// Removals name valid, distinct columns.
		`{"request":{"dropColumns":["a","a"]}}`,
		`{"request":{"dropColumns":[""]}}`,
		`{"request":{"dropColumns":["a/b"]}}`,
		// An addition or a removal holds nothing.
		`{"request":{"dropColumns":["a"]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}]},"held_families":[{"name":"f"}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c := qt.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.AlterOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

func TestColumnOperation_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		impact  protocol.ChangeSemantics_Impact
	}{
		{"addition", `{"request":{"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false},{"name":"b","type":{"optionalType":{"item":{"decimalType":{"precision":22,"scale":9}}}},"notNull":false}]}}`, protocol.ChangeSemantics_ADDITIVE},
		{"removal", `{"request":{"dropColumns":["a","b"]}}`, protocol.ChangeSemantics_DESTRUCTIVE},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.AlterOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.IsNil)
			c.Assert(normalized.OperationSemantics.Impact, qt.Equals, test.impact)
		})
	}
}
