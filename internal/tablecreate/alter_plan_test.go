package tablecreate_test

import (
	"encoding/json"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// alterCase states a table's families and column placements on both sides.
// Declared and held columns are named without the key column "id", which every
// table has.
type alterCase struct {
	name           string
	desiredTable   string
	desiredColumns map[string]string
	declared       []string
	// types gives a declared column a type other than Utf8.
	types       map[string]string
	heldTable   string
	heldColumns map[string]string
	held        []string
	want        string
	// desiredIndexes and heldIndexes are the index sets of both sides; empty
	// states none.
	desiredIndexes, heldIndexes string
	// desiredKnowledge is what the declaring source established about the table
	// properties of every table it declares; zero states nothing.
	desiredKnowledge protocol.ModelKnowledge_State
	// desiredSubjectKnowledge is what the source established about this table
	// alone: an absence or a request for defaults, which only a subject states.
	desiredSubjectKnowledge protocol.ModelKnowledge_State
}

const plainTable = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}]}`

func observedFacet(c *qt.C, kind, name, payload string) *protocol.ComparisonRecord {
	c.Helper()
	reference := owner()
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == kind {
			model = candidate
		}
	}
	if kind == schema.ColumnProperties {
		reference.Kind, reference.Path, reference.Name = "column", []string{reference.Name}, name
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: kind + name, Representation: protocol.ValueInput_OBSERVED, Value: &protocol.Envelope{ProviderId: schema.ProviderID,
			Kind: kind, SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: reference, Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	identity := &protocol.ComparisonIdentity{Kind: reference.Kind, Database: part(reference.Database), Schema: part(reference.Schema), Name: part(reference.Name)}
	if reference.Kind == "column" {
		identity.Parent = part("records")
	}
	return &protocol.ComparisonRecord{Kind: kind, Subject: identity, Value: values.Values[0]}
}

func tableSubject() *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("records")}
}

// alterRequest captures an ALTER of app.records with the families of both sides.
func alterRequest(c *qt.C, in alterCase) *protocol.PlanFeaturesRequest {
	c.Helper()
	desired := &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}},
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
	current := &protocol.ObservedTable{Name: "records", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "id"}},
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
			Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}}}
	if in.desiredKnowledge != protocol.ModelKnowledge_STATE_UNSPECIFIED {
		desired.Features.Facets.Coverage.Kinds = append(desired.Features.Facets.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: schema.TableDesiredModel(),
			Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: in.desiredKnowledge, Reason: "fixture"}}}})
	}
	if in.desiredSubjectKnowledge != protocol.ModelKnowledge_STATE_UNSPECIFIED {
		desired.Features.Facets.Coverage.Kinds = append(desired.Features.Facets.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: schema.TableDesiredModel(),
			Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "fixture"}}}})
		desired.Features.Facets.Coverage.Subjects = append(desired.Features.Facets.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: schema.TableProperties,
			Subject: tableSubject(), Knowledge: &protocol.ModelKnowledge{State: in.desiredSubjectKnowledge, Reason: "fixture"}})
	}
	for _, name := range in.declared {
		kind := "Utf8"
		if declared, found := in.types[name]; found {
			kind = declared
		}
		desired.Columns = append(desired.Columns, &protocol.DeclaredColumn{Name: name, Type: kind, Nullable: true})
	}
	for _, name := range in.held {
		current.Columns = append(current.Columns, &protocol.ObservedColumn{Name: name})
	}
	if in.desiredTable != "" {
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, facet(c, schema.TableProperties, "", in.desiredTable))
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range in.desiredColumns {
			if !yield(name) {
				return
			}
		}
	}) {
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, facet(c, schema.ColumnProperties, name, in.desiredColumns[name]))
	}
	if in.desiredIndexes != "" {
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, facet(c, schema.IndexProperties, "", in.desiredIndexes))
	}
	if in.heldIndexes != "" {
		current.Features.Facets.Records = append(current.Features.Facets.Records, observedFacet(c, schema.IndexProperties, "", in.heldIndexes))
	}
	if in.heldTable != "" {
		current.Features.Facets.Records = append(current.Features.Facets.Records, observedFacet(c, schema.TableProperties, "", in.heldTable))
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range in.heldColumns {
			if !yield(name) {
				return
			}
		}
	}) {
		current.Features.Facets.Records = append(current.Features.Facets.Records, observedFacet(c, schema.ColumnProperties, name, in.heldColumns[name]))
	}
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, DatabasePath: "/local", ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false},
		Tables:       []*protocol.PlanningTable{{Subject: tableSubject(), Action: protocol.PlanningTable_ALTER, Desired: desired, Current: current}}}
}

func TestAlterPlan_HappyPath(t *testing.T) {
	for _, test := range []alterCase{
		{name: "adds a family with its settings",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4","data":{"media":"hdd"}}]}`,
			heldTable:    plainTable,
			want:         `{"addColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4","data":{"media":"hdd"}}]}`},
		{name: "changes only the settings that differ",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4","data":{"media":"hdd"}}]}`,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_NONE","data":{"media":"hdd"}}]}`,
			want:         `{"alterColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`},
		{name: "changes the storage pool",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","data":{"media":"ssd"}}]}`,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive","data":{"media":"hdd"}}]}`,
			want:         `{"alterColumnFamilies":[{"name":"archive","data":{"media":"ssd"}}]}`},
		{name: "changes the default family",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default","compression":"COMPRESSION_LZ4"}]}`,
			heldTable:    plainTable,
			want:         `{"alterColumnFamilies":[{"name":"default","compression":"COMPRESSION_LZ4"}]}`},
		{name: "moves a column into a family the table holds",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`}, declared: []string{"payload"},
			heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`, held: []string{"payload"},
			want: `{"alterColumns":[{"name":"payload","family":"archive"}]}`},
		{name: "moves a column back to the default family",
			desiredColumns: map[string]string{"payload": `{}`}, declared: []string{"payload"},
			heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`, heldColumns: map[string]string{"payload": `{"family":"archive"}`}, held: []string{"payload"},
			want: `{"alterColumns":[{"name":"payload","family":"default"}]}`},
		{name: "adds a family and moves a column into it",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`}, declared: []string{"payload"},
			heldTable: plainTable, held: []string{"payload"},
			want: `{"addColumnFamilies":[{"name":"archive"}],"alterColumns":[{"name":"payload","family":"archive"}]}`},
		{name: "moves a column the plan adds",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`}, declared: []string{"payload"},
			heldTable: plainTable,
			want:      `{"addColumnFamilies":[{"name":"archive"}],"alterColumns":[{"name":"payload","family":"archive"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, test)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 5)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "row-table-alter/000000")
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.AlterOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 1)
			c.Assert(plan.Parents[1].Receipt.Steps, qt.HasLen, 1)
			c.Assert(plan.Parents[2].Receipt.Steps, qt.HasLen, 1)
			alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			got, err := protojson.Marshal(alteration.Request)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.want))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func jsonValue(c *qt.C, text string) any {
	c.Helper()
	var value any
	c.Assert(jsonUnmarshal(text, &value), qt.IsNil)
	return value
}

func TestAlterPlan_NoChange(t *testing.T) {
	for _, test := range []alterCase{
		{name: "nothing declared"},
		{name: "families already match",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`},
		{name: "a setting the declaration leaves out is kept",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4","data":{"media":"ssd"}}]}`},
		{name: "declared uncompressed matches an unspecified compression",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_NONE"}]}`,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`},
		{name: "a family only the table holds stays",
			desiredTable: plainTable,
			heldTable:    `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"profile","compression":"COMPRESSION_LZ4"}]}`},
		{name: "a column without declared placement keeps its family",
			declared: []string{"payload"}, heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`,
			heldColumns: map[string]string{"payload": `{"family":"archive"}`}, held: []string{"payload"}},
		{name: "a column in the declared family stays",
			desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`}, declared: []string{"payload"},
			heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`, heldColumns: map[string]string{"payload": `{"family":"archive"}`}, held: []string{"payload"}},
		{name: "a dropped column is never moved",
			desiredTable: plainTable, declared: nil,
			heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`, heldColumns: map[string]string{"gone": `{"family":"archive"}`}, held: []string{"gone"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, test)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 5)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
			c.Assert(plan.Parents[0].Action, qt.Equals, protocol.PlanningTable_ALTER)
		})
	}
}

func TestAlterPlan_FailurePath(t *testing.T) {
	archive := `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"key column in another family", alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"id": `{"family":"archive"}`}, heldTable: archive, held: nil},
			`.*key column "id".*default family.*`},
		{"family neither declared nor held", alterCase{desiredColumns: map[string]string{"payload": `{"family":"cold"}`}, declared: []string{"payload"}, heldTable: archive, held: []string{"payload"}},
			`.*column "payload" names column family "cold".*`},
		{"keep_in_memory on one side", alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","keepInMemory":"ENABLED"}]}`, heldTable: archive},
			`.*keeps its columns in memory.*`},
		{"current families not captured", alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`},
			`The table's current column families were not captured`},
		{"properties of an undeclared column", alterCase{desiredColumns: map[string]string{"ghost": `{"family":"archive"}`}, heldTable: archive},
			`Column properties name column "ghost".*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 0)
		})
	}
}

func TestAlterPlan_RefusesLifecyclesItDoesNotOwn(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*protocol.PlanFeaturesRequest)
		want   string
	}{
		{"rebuild", func(r *protocol.PlanFeaturesRequest) { r.Tables[0].Action = protocol.PlanningTable_REBUILD },
			"Only native row-table creation, alteration of column families, columns, the TTL, the partitioning settings and the indexes, column-table creation and table removal are implemented"},
		{"transactional claim", func(r *protocol.PlanFeaturesRequest) { r.Capabilities["transactional_ddl"] = true },
			"YDB table creation requires established nontransactional DDL"},
		{"unmeasured capabilities", func(r *protocol.PlanFeaturesRequest) { r.Capabilities = nil },
			"YDB table creation requires established nontransactional DDL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, heldTable: plainTable})
			test.modify(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.want)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func commonStep(name string, parent *protocol.ComparisonIdentity, added string, effects ...*protocol.PlanningEffect) *protocol.CommonPlanningStep {
	step := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: name}, Parent: proto.CloneOf(parent),
		Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: effects}}
	if added != "" {
		step.AddedColumn = &protocol.PlanningColumn{Name: added, Type: "Utf8", Nullable: true}
	}
	return step
}

func TestAlterPlan_OrdersAgainstColumnSteps(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{
		desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`}, declared: []string{"payload", "other"},
		heldTable: plainTable})
	subject := request.Tables[0].Subject
	column := &protocol.ComparisonIdentity{Kind: "column", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part("old")}
	request.CommonSteps = []*protocol.CommonPlanningStep{
		commonStep("add-payload", subject, "payload"),
		commonStep("add-other", subject, "other"),
		commonStep("drop-old", subject, "", &protocol.PlanningEffect{Subject: column, Action: protocol.PlanningEffect_DROP}),
		commonStep("alter-ttl", subject, "", &protocol.PlanningEffect{Subject: subject, Action: protocol.PlanningEffect_ALTER}),
	}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	// The removal has a native replacement; the additions carry no footprint, so the
	// host keeps them and the family step names them by their host identity.
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"drop-old => row-table-drop-column/000000"})
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 2)
	step := plan.Contributions[0].Steps[1].Id
	c.Assert(step.Name, qt.Equals, "row-table-alter/000000")
	c.Assert(edges(plan), qt.DeepEquals, []string{"add-payload -> " + step.Name, step.Name + " -> row-table-drop-column/000000"})
}

func TestAlterPlan_LeavesColumnStepsAloneWithoutFamilyChange(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable})
	request.CommonSteps = []*protocol.CommonPlanningStep{commonStep("add-payload", request.Tables[0].Subject, "payload")}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(plan.Rewrites, qt.HasLen, 0)
}

func TestAlterPlan_RenderAndNormalizeRoundTrip(t *testing.T) {
	c := qt.New(t)
	service := backend.New()
	request := alterRequest(c, alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, heldTable: plainTable})
	plan, err := service.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := plan.Contributions[0].Steps[0].Operation.Value
	normalized, err := service.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: value.Id, Representation: protocol.ValueInput_OPERATION, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], value), qt.IsTrue)
	c.Assert(value.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_ADDITIVE)
	rendered, err := service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: proto.CloneOf(value)}}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(proto.Equal(rendered.Units[0].GetNative(), value.Value), qt.IsTrue)
	forged := proto.CloneOf(value)
	forged.OperationSemantics.Impact = protocol.ChangeSemantics_DESTRUCTIVE
	_, err = service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: forged}}}}})
	c.Assert(err, qt.IsNotNil)
}

func TestAlterPlan_MovesAreBehavioral(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, desiredColumns: map[string]string{"payload": `{"family":"archive"}`},
		declared: []string{"payload"}, heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`, held: []string{"payload"}})
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions[0].Steps[0].Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(plan.Contributions[0].Steps[0].Operation.Value.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
}

func TestAlterOperation_FailurePath(t *testing.T) {
	for _, payload := range []string{
		`{"request":{"sessionId":"smuggled","alterColumns":[{"name":"a","family":"f"}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"path":"/other/table","alterColumns":[{"name":"a","family":"f"}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"dropColumns":[]}}`,
		`{"request":{}}`,
		`{"request":{"alterColumns":[{"name":"a","family":null}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f","type":{"typeId":"UINT64"}}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f"}]}}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f"}]},"held_columns":[{"name":"a","family":""},{"name":"b","family":""}]}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f"},{"name":"a","family":"g"}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"addColumnFamilies":[{"name":"f","keepInMemory":"ENABLED"}]}}`,
		`{"request":{"addColumnFamilies":[{"name":"f"},{"name":"f"}]}}`,
		`{"request":{"alterColumnFamilies":[{"name":"f"}]},"held_families":[{"name":"f"}]}`,
		`{"request":{"alterColumnFamilies":[{"name":"f","compression":"COMPRESSION_LZ4"}]}}`,
		`{"request":{"addColumnFamilies":[{"name":"f"}]},"unknown":true}`,
		`{"request":{"addColumnFamilies":[{"name":"f/g"}]}}`,
		`{"request":{"addColumnFamilies":[{"name":"f","compression":null}]}}`,
		`{"request":{"alterColumnFamilies":[{"name":"f","compression":"COMPRESSION_LZ4"}]},"held_families":[{"name":"f"},{"name":"g"}]}`,
		`{"request":{"alterColumnFamilies":[{"name":"f","compression":"COMPRESSION_LZ4"}]},"held_families":[{"name":"g"}]}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f"}]},"held_columns":[{"name":"b","family":""}]}`,
		`{"request":{"alterColumns":[{"name":"a"}]},"held_columns":[{"name":"a","family":""}]}`,
		`{"request":{"alterColumns":[{"name":"a","family":"f/g"}]},"held_columns":[{"name":"a","family":""}]}`,
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

func TestAlterOperation_RefusesAnotherModel(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, heldTable: plainTable})
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := proto.CloneOf(plan.Contributions[0].Steps[0].Operation.Value.Value)
	value.DefinitionSha256 = tablecreate.Model().DefinitionSha256
	normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: value})
	c.Assert(err, qt.ErrorMatches, "YDB table alteration does not match its negotiated operation model")
	c.Assert(normalized, qt.IsNil)
}

func jsonUnmarshal(text string, target any) error { return json.Unmarshal([]byte(text), target) }

func TestAlterPlan_ExcludedAndForeignFacets(t *testing.T) {
	base := alterCase{desiredTable: plainTable, heldTable: plainTable}
	for _, test := range []struct {
		name   string
		modify func(*qt.C, *protocol.PlanFeaturesRequest)
	}{
		{"excluded desired table facet", func(c *qt.C, r *protocol.PlanFeaturesRequest) {
			state := r.Tables[0].Desired.Features.Facets
			state.Records = []*protocol.ComparisonRecord{{Kind: schema.TableProperties, Subject: tableSubject(), Targets: []string{"ydb"}}}
		}},
		{"desired facet for another target", func(c *qt.C, r *protocol.PlanFeaturesRequest) {
			record := facet(c, schema.TableProperties, "", `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`)
			record.Targets = []string{"postgres"}
			r.Tables[0].Desired.Features.Facets.Records = []*protocol.ComparisonRecord{record}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, base)
			test.modify(c, request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestAlterPlan_RefusesFacetsOfOtherOwners(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*qt.C, *protocol.PlanFeaturesRequest)
	}{
		{"duplicate desired table facet", func(c *qt.C, r *protocol.PlanFeaturesRequest) {
			state := r.Tables[0].Desired.Features.Facets
			state.Records = append(state.Records, proto.CloneOf(state.Records[0]))
		}},
		{"desired facet of another table", func(c *qt.C, r *protocol.PlanFeaturesRequest) {
			other := proto.CloneOf(r.Tables[0].Desired.Features.Facets.Records[0])
			other.Value.Value.Owner.Name = "elsewhere"
			other.Value.NormalizedOwner.Name = "elsewhere"
			other.Subject.Name = part("elsewhere")
			r.Tables[0].Desired.Features.Facets.Records = []*protocol.ComparisonRecord{other}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable})
			test.modify(c, request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(plan, qt.IsNil)
		})
	}
}

func TestAlterOperation_RefusesInvalidOwner(t *testing.T) {
	for _, bad := range []*protocol.Reference{
		{Engine: "ydb", Kind: "table", Schema: "app", Name: ""},
		{Engine: "ydb", Kind: "table", Schema: "app/../other", Name: "records"},
		{Engine: "ydb", Kind: "table", Database: "local", Schema: "app", Name: "records"},
		{Engine: "ydb", Kind: "table", Schema: "", Name: "ptah_locks"},
		{Engine: "ydb", Kind: "table", Schema: "", Name: "ptah_dev"},
		{Engine: "ydb", Kind: "table", Schema: "ptah_dev/r1", Name: "records"},
	} {
		t.Run(bad.String(), func(t *testing.T) {
			c := qt.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.AlterOperationKind, Owner: bad, SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(`{"request":{"addColumnFamilies":[{"name":"f"}]}}`)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
