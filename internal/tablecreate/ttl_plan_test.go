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

// ttlTable is a table whose TTL setting is the given JSON object.
func ttlTable(ttl string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"ttlSettings":` + ttl + `}`
}

const (
	hourly      = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}`
	hourlyEpoch = `{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":3600}}`
)

var ttlTypes = map[string]string{"ts": "Timestamp", "day": "Date", "stamp": "Uint64", "name": "Utf8", "count": "Int64"}

func ttlCase(desired, held string, columns ...string) alterCase {
	return alterCase{desiredTable: desired, heldTable: held, declared: columns, types: ttlTypes, held: columns}
}

func TestTTLPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		in       alterCase
		want     string
		wantHeld string
		effect   protocol.PlanningEffect_Action
	}{
		{"sets a TTL on a table without one",
			ttlCase(ttlTable(hourly), plainTable, "ts"),
			`{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`, `{}`, protocol.PlanningEffect_CREATE},
		{"changes the interval",
			ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":7200}}`), ttlTable(hourly), "ts"),
			`{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":7200}}}`, hourly, protocol.PlanningEffect_ALTER},
		{"moves the TTL to another column",
			ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"day","expireAfterSeconds":3600}}`), ttlTable(hourly), "ts", "day"),
			`{"setTtlSettings":{"dateTypeColumn":{"columnName":"day","expireAfterSeconds":3600}}}`, hourly, protocol.PlanningEffect_ALTER},
		{"switches from a date column to an integer column",
			ttlCase(ttlTable(hourlyEpoch), ttlTable(hourly), "ts", "stamp"),
			`{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":3600}}}`, hourly, protocol.PlanningEffect_ALTER},
		{"changes the unit of an integer column",
			ttlCase(ttlTable(`{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_SECONDS","expireAfterSeconds":3600}}`), ttlTable(hourlyEpoch), "stamp"),
			`{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_SECONDS","expireAfterSeconds":3600}}}`, hourlyEpoch, protocol.PlanningEffect_ALTER},
		{"removes the TTL a declaration leaves out",
			ttlCase(plainTable, ttlTable(hourly), "ts"),
			`{"dropTtlSettings":{}}`, hourly, protocol.PlanningEffect_DROP},
		{"keeps the run interval the table held",
			ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":7200}}`), ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`), "ts"),
			`{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":7200},"runIntervalSeconds":900}}`,
			`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`, protocol.PlanningEffect_ALTER},
		{"states the run interval the declaration asks for",
			ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":1800}`), ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`), "ts"),
			`{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":1800}}`,
			`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`, protocol.PlanningEffect_ALTER},
		{"reads the integer column of a key",
			ttlCase(ttlTable(`{"valueSinceUnixEpoch":{"columnName":"id","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`), plainTable),
			`{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"id","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}}`, `{}`, protocol.PlanningEffect_CREATE},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			in := test.in
			in.types = map[string]string{"id": "Uint64", "ts": "Timestamp", "day": "Date", "stamp": "Uint64"}
			request := alterRequest(c, in)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "row-table-ttl/000000")
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.AlterOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Operation.Value.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			c.Assert(step.Metadata.Effects[1].Action, qt.Equals, test.effect)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.ttl")
			c.Assert(plan.Parents, qt.HasLen, 3)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 1)
			c.Assert(plan.Parents[0].Receipt.Steps[0].Name, qt.Equals, "row-table-ttl/000000")
			alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			got, err := protojson.Marshal(alteration.Request)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.want))
			held, err := protojson.Marshal(alteration.HeldTTL)
			c.Assert(err, qt.IsNil)
			c.Assert(string(held), qt.JSONEquals, jsonValue(c, test.wantHeld))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func TestTTLPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"neither side has a TTL", ttlCase(plainTable, plainTable)},
		{"the table holds the declared TTL", ttlCase(ttlTable(hourly), ttlTable(hourly), "ts")},
		{"the table holds the declared integer TTL", ttlCase(ttlTable(hourlyEpoch), ttlTable(hourlyEpoch), "stamp")},
		{"a run interval the declaration leaves out is not compared",
			ttlCase(ttlTable(hourly), ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`), "ts")},
		{"a declared run interval the table holds is not a change",
			ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`), ttlTable(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`), "ts")},
		{"a declaration without table properties leaves the TTL alone", ttlCase("", ttlTable(hourly), "ts")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 3)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

func TestTTLPlan_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"column the table does not declare", ttlCase(ttlTable(hourly), plainTable), `the TTL reads column "ts", which the table does not declare`},
		{"a held TTL column the declaration no longer states", ttlCase(ttlTable(hourly), ttlTable(hourly)), `the TTL reads column "ts", which the table does not declare`},
		{"date mode on an integer column", ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"stamp","expireAfterSeconds":60}}`), plainTable, "stamp"),
			`the TTL column "stamp" is Uint64, an integer type, and YDB needs the unit.*`},
		{"unit on a date column", ttlCase(ttlTable(`{"valueSinceUnixEpoch":{"columnName":"ts","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`), plainTable, "ts"),
			`the TTL column "ts" is Timestamp, a date type, and YDB takes no unit.*`},
		{"text column", ttlCase(ttlTable(`{"dateTypeColumn":{"columnName":"name","expireAfterSeconds":60}}`), plainTable, "name"),
			`the TTL column "name" is Utf8, and YDB reads a TTL from.*`},
		{"signed integer column", ttlCase(ttlTable(`{"valueSinceUnixEpoch":{"columnName":"count","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`), plainTable, "count"),
			`the TTL column "count" is Int64, and YDB reads a TTL from.*`},
		{"current TTL not captured", alterCase{desiredTable: `{"primaryKey":["id"],"ttlSettings":` + hourly + `}`, declared: []string{"ts"}, types: ttlTypes}, `The table's current TTL was not captured`},
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

// ttlStepRequest alters app.records to move its TTL from the column "old" to
// the column "fresh", with the host steps that add "fresh" and drop "old".
func ttlStepRequest(c *qt.C, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	c.Helper()
	return columnRequest(c, alterCase{
		desiredTable: ttlTable(`{"dateTypeColumn":{"columnName":"fresh","expireAfterSeconds":3600}}`), declared: []string{"fresh", "keep", "extra"},
		types:     map[string]string{"fresh": "Timestamp"},
		heldTable: ttlTable(`{"dateTypeColumn":{"columnName":"old","expireAfterSeconds":3600}}`), held: []string{"old", "keep"}}, steps...)
}

func TestTTLPlan_OrdersAgainstColumnSteps(t *testing.T) {
	timestamp := func(column *protocol.PlanningColumn) { column.Type = "Timestamp" }
	for _, test := range []struct {
		name  string
		steps []*protocol.CommonPlanningStep
		edges []string
	}{
		{"follows the addition of its column and precedes the removal of the column it read",
			[]*protocol.CommonPlanningStep{removalStep("drop-old", "old"), additionStep("add-fresh", "fresh", timestamp)},
			[]string{"row-table-ttl/000000 -> row-table-drop-column/000000", "row-table-add-column/000000 -> row-table-ttl/000000"}},
		{"names a step the host keeps by its host identity",
			[]*protocol.CommonPlanningStep{removalStep("drop-old", "old"), additionStep("add-fresh", "fresh", func(column *protocol.PlanningColumn) {
				column.Type, column.Nullable = "Timestamp", false
			})},
			[]string{"row-table-ttl/000000 -> row-table-drop-column/000000", "add-fresh -> row-table-ttl/000000"}},
		{"ignores the steps of other columns",
			[]*protocol.CommonPlanningStep{removalStep("drop-unrelated", "unrelated"), additionStep("add-extra", "extra", nil)}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), ttlStepRequest(c, test.steps...))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(edges(plan), qt.DeepEquals, test.edges)
		})
	}
}

func TestTTLPlan_OrdersAgainstAColumnStepWithoutAFootprint(t *testing.T) {
	c := qt.New(t)
	// A removal the footprint does not name can remove the TTL column, so the TTL goes first.
	opaque := commonStep("drop-something", tableSubject(), "", &protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: columnSubject("old"), Action: protocol.PlanningEffect_DROP}, &protocol.PlanningEffect{Subject: columnSubject("keep"), Action: protocol.PlanningEffect_DROP})
	plan, err := backend.New().PlanFeatures(c.Context(), ttlStepRequest(c, opaque))
	c.Assert(err, qt.IsNil)
	c.Assert(edges(plan), qt.DeepEquals, []string{"row-table-ttl/000000 -> drop-something"})
}

func TestTTLPlan_RemovalPrecedesTheRemovalOfItsColumn(t *testing.T) {
	c := qt.New(t)
	request := columnRequest(c, alterCase{desiredTable: plainTable, declared: []string{"keep"}, heldTable: ttlTable(`{"dateTypeColumn":{"columnName":"old","expireAfterSeconds":3600}}`), held: []string{"old", "keep"}},
		removalStep("drop-old", "old"), removalStep("drop-other", "other"))
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"drop-old => row-table-drop-column/000000", "drop-other => row-table-drop-column/000001"})
	c.Assert(edges(plan), qt.DeepEquals, []string{"row-table-ttl/000000 -> row-table-drop-column/000000"})
	c.Assert(plan.Parents[0].Receipt.Steps[0].Name, qt.Equals, "row-table-ttl/000000")
}

// accountsTable is a second table whose TTL moves from nothing to hourly.
func accountsTable(c *qt.C) *protocol.PlanningTable {
	c.Helper()
	reference := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "accounts"}
	normalize := func(representation protocol.ValueInput_Representation, model *protocol.ValueModel, payload string) *protocol.ComparisonRecord {
		values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
			{Id: "accounts", Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties,
				SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(reference), Payload: []byte(payload)}}}})
		c.Assert(err, qt.IsNil)
		return &protocol.ComparisonRecord{Kind: schema.TableProperties, Value: values.Values[0],
			Subject: &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("accounts")}}
	}
	var observed *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == schema.TableProperties {
			observed = model
		}
	}
	return &protocol.PlanningTable{Subject: &protocol.ComparisonIdentity{Kind: "table", Database: part("/local"), Schema: part("app"), Name: part("accounts")},
		Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "accounts", Schema: "app", PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
					Records: []*protocol.ComparisonRecord{normalize(protocol.ValueInput_DESIRED, schema.TableDesiredModel(), ttlTable(hourly))}}}},
		Current: &protocol.ObservedTable{Name: "accounts", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "id"}, {Name: "ts"}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
				Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{},
					Records: []*protocol.ComparisonRecord{normalize(protocol.ValueInput_OBSERVED, observed, plainTable)}}}}}
}

func TestTTLPlan_NumbersStepsAcrossTables(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, ttlCase(ttlTable(hourly), plainTable, "ts"))
	request.Tables = append(request.Tables, accountsTable(c))
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 2)
	// Tables are planned in identity order, and the numbers stay unique across them.
	c.Assert(plan.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-ttl/000000")
	c.Assert(plan.Contributions[0].Steps[0].Operation.Value.Value.Owner.Name, qt.Equals, "accounts")
	c.Assert(plan.Contributions[0].Steps[1].Id.Name, qt.Equals, "row-table-ttl/000001")
	c.Assert(plan.Contributions[0].Steps[1].Operation.Value.Value.Owner.Name, qt.Equals, "records")
}

func TestTTLPlan_OrdersOnlyAgainstStepsOfItsOwnTable(t *testing.T) {
	c := qt.New(t)
	request := ttlStepRequest(c)
	request.Tables = append(request.Tables, accountsTable(c))
	accounts := request.Tables[1].Subject
	// accounts adds the column its own TTL reads, and drops one named like records' old TTL column.
	add := additionStep("add-ts", "ts", func(column *protocol.PlanningColumn) { column.Type = "Timestamp" })
	add.Parent = accounts
	add.Metadata.Effects[0].Subject = accounts
	add.Metadata.Effects[1].Subject.Parent = part("accounts")
	drop := removalStep("drop-old", "old")
	drop.Parent = accounts
	drop.Metadata.Effects[0].Subject = accounts
	drop.Metadata.Effects[1].Subject.Parent = part("accounts")
	request.CommonSteps = []*protocol.CommonPlanningStep{add, drop, additionStep("add-fresh", "fresh", func(column *protocol.PlanningColumn) { column.Type = "Timestamp" })}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	// accounts is numbered first. Its TTL follows its own addition; records' TTL follows only records' addition.
	c.Assert(edges(plan), qt.DeepEquals, []string{"row-table-add-column/000000 -> row-table-ttl/000000", "row-table-add-column/000001 -> row-table-ttl/000001"})
}

func TestTTLPlan_FamilyAndTTLAreSeparateSteps(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{
		desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}],"ttlSettings":` + hourly + `}`, declared: []string{"ts"}, types: ttlTypes,
		heldTable: plainTable, held: []string{"ts"}})
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	var names []string
	for _, step := range plan.Contributions[0].Steps {
		names = append(names, step.Id.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"row-table-alter/000000", "row-table-ttl/000000"})
	c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 2)
}

func TestTTLPlan_RoundTrip(t *testing.T) {
	c := qt.New(t)
	service := backend.New()
	request := alterRequest(c, ttlCase(plainTable, ttlTable(hourly), "ts"))
	plan, err := service.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := plan.Contributions[0].Steps[0].Operation.Value
	normalized, err := service.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: value.Id, Representation: protocol.ValueInput_OPERATION, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], value), qt.IsTrue)
	rendered, err := service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: proto.CloneOf(value)}}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	forged := proto.CloneOf(value)
	forged.OperationSemantics.Impact = protocol.ChangeSemantics_ADDITIVE
	_, err = service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: forged}}}}})
	c.Assert(err, qt.IsNotNil)
}

func TestTTLOperation_FailurePath(t *testing.T) {
	const set = `"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60}}`
	for _, test := range []struct {
		name    string
		payload string
	}{
		{"no mode", `{"request":{"setTtlSettings":{}}}`},
		{"empty column", `{"request":{"setTtlSettings":{"dateTypeColumn":{"columnName":"","expireAfterSeconds":60}}}}`},
		{"column with a separator", `{"request":{"setTtlSettings":{"dateTypeColumn":{"columnName":"a/b","expireAfterSeconds":60}}}}`},
		{"epoch without a unit", `{"request":{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"ts","expireAfterSeconds":60}}}}`},
		{"epoch with an unnamed unit", `{"request":{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"ts","columnUnit":9,"expireAfterSeconds":60}}}}`},
		{"epoch with an unknown unit", `{"request":{"setTtlSettings":{"valueSinceUnixEpoch":{"columnName":"ts","columnUnit":"UNIT_FORTNIGHTS","expireAfterSeconds":60}}}}`},
		{"null settings", `{"request":{"setTtlSettings":null}}`},
		{"unknown field in the mode", `{"request":{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60,"extra":1}}}}`},
		{"removal without the held TTL", `{"request":{"dropTtlSettings":{}}}`},
		{"a TTL set that changes nothing", `{"request":{` + set + `},"held_ttl":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60}}}`},
		{"held TTL without a TTL action", `{"request":{"dropColumns":["a"]},"held_ttl":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60}}}`},
		{"invalid held TTL", `{"request":{` + set + `},"held_ttl":{}}`},
		{"TTL and column removal", `{"request":{` + set + `,"dropColumns":["a"]}}`},
		{"TTL and family change", `{"request":{` + set + `,"addColumnFamilies":[{"name":"f"}]}}`},
		{"TTL and column addition", `{"request":{` + set + `,"addColumns":[{"name":"a","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}]}}`},
		{"set and removal at once", `{"request":{` + set + `,"dropTtlSettings":{}},"held_ttl":{"dateTypeColumn":{"columnName":"x","expireAfterSeconds":60}}}`},
		{"tiering smuggled in", `{"request":{` + set + `,"dropTiering":{}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: "github.com/stokaro/ptah-ydb-plugin", Kind: tablecreate.AlterOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

func TestTTLPlan_ColumnTypes(t *testing.T) {
	for _, test := range []struct {
		kind string
		ttl  string
	}{
		{"Date", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Datetime", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Timestamp", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Date32", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Datetime64", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Timestamp64", `{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`},
		{"Uint32", `{"valueSinceUnixEpoch":{"columnName":"c","columnUnit":"UNIT_NANOSECONDS","expireAfterSeconds":60}}`},
		{"Uint64", `{"valueSinceUnixEpoch":{"columnName":"c","columnUnit":"UNIT_MICROSECONDS","expireAfterSeconds":60}}`},
		{"DyNumber", `{"valueSinceUnixEpoch":{"columnName":"c","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`},
	} {
		t.Run(test.kind, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, alterCase{desiredTable: ttlTable(test.ttl), heldTable: plainTable,
				declared: []string{"c"}, types: map[string]string{"c": test.kind}, held: []string{"c"}}))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
		})
	}
}

func TestTTLPlan_RefusedColumnTypes(t *testing.T) {
	for _, kind := range []string{"Bool", "Int32", "Int64", "Uint8", "Uint16", "Utf8", "String", "Interval", "Double", "Json"} {
		t.Run(kind, func(t *testing.T) {
			c := qt.New(t)
			for _, ttl := range []string{`{"dateTypeColumn":{"columnName":"c","expireAfterSeconds":60}}`,
				`{"valueSinceUnixEpoch":{"columnName":"c","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`} {
				plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, alterCase{desiredTable: ttlTable(ttl), heldTable: plainTable,
					declared: []string{"c"}, types: map[string]string{"c": kind}, held: []string{"c"}}))
				c.Assert(err, qt.IsNil)
				c.Assert(plan.Diagnostics, qt.HasLen, 1)
				c.Assert(plan.Contributions, qt.HasLen, 0)
			}
		})
	}
}
