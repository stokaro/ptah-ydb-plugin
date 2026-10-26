package backend_test

import (
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// external is one external object of app: a data source or a table.
type external struct {
	name   string
	source *ydbdecl.ExternalDataSource
	table  *ydbdecl.ExternalTable
}

func (e external) kind() string {
	if e.source != nil {
		return schema.ExternalSourceProperties
	}
	return schema.ExternalTableProperties
}

func bucketAt(location string) *ydbdecl.ExternalDataSource {
	return &ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: location, AuthMethod: "NONE"}
}

func eventsOver(source string, columns ...string) *ydbdecl.ExternalTable {
	table := &ydbdecl.ExternalTable{DataSource: source, Location: "2026/", Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Int64", NotNull: true}},
		Options: map[string]string{"FORMAT": "csv_with_names", "CSV_DELIMITER": ";"}}
	for _, column := range columns {
		table.Columns = append(table.Columns, ydbdecl.ExternalColumn{Name: column, Type: "Utf8"})
	}
	return table
}

// readExternal is an object as YDB 26.2.1.14 describes the one a statement
// made from its declaration: the data source's options in its properties
// with REFERENCES beside them, the table's data source absolute and each of
// its options a JSON array.
func readExternal(c *qt.C, object external) proto.Message {
	c.Helper()
	self := &Ydb_Scheme.Entry{Owner: "root@builtin", Permissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.granular.describe_schema"}}}}
	if object.source != nil {
		intent, err := externals.SourceIntent(*object.source)
		c.Assert(err, qt.IsNil)
		properties := intent.GetProperties()
		properties["REFERENCES"] = `["/local/app/events"]`
		return &Ydb_Table.DescribeExternalDataSourceResult{Self: self, SourceType: intent.SourceType, Location: intent.Location, Properties: properties}
	}
	intent, err := externals.TableIntent(*object.table)
	c.Assert(err, qt.IsNil)
	described := &native.DescribeExternalTableResult{Self: self, SourceType: intent.SourceType, DataSourcePath: new("/local/" + object.table.DataSource), Location: intent.Location, Content: intent.Content}
	for _, column := range intent.Columns {
		described.Columns = append(described.Columns, &native.ColumnMeta{Name: column.Name, Type: column.Type, NotNull: column.NotNull})
	}
	return described
}

func externalSubject(kind, database, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part(database), Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
}

// externalRequest captures a comparison of the external objects of app the
// way the host sends it: the declarations named without a database, the
// read objects and the read directory with the session's.
func externalRequest(c *qt.C, declared, read []external, capabilities map[string]bool) *protocol.CompareModelsRequest {
	c.Helper()
	observed := map[string]*protocol.ValueModel{}
	for _, model := range schema.ObservedModels() {
		observed[model.Kind] = model
	}
	desired := &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}
	current := &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}
	for _, kind := range externalcompare.Kinds() {
		desired.Coverage.Kinds = append(desired.Coverage.Kinds, testcomparison.State(schema.NativeDesiredModel(kind)).Coverage.Kinds...)
		state := testcomparison.State(observed[kind])
		state.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
		state.Coverage.Kinds[0].Sources[0].Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Targets: []string{"ydb"}, SubjectKinds: []string{kind},
			Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""), Schema: testcomparison.Part("app")}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
		current.Coverage.Kinds = append(current.Coverage.Kinds, state.Coverage.Kinds...)
	}
	for i, object := range declared {
		owner := &protocol.Reference{Engine: "ydb", Kind: object.kind(), Schema: "app", Name: object.name}
		var intent proto.Message
		var err error
		if object.source != nil {
			intent, err = externals.SourceIntent(*object.source)
		} else {
			intent, err = externals.TableIntent(*object.table)
		}
		c.Assert(err, qt.IsNil)
		value, err := schema.CaptureExternalIntent(owner, intent)
		c.Assert(err, qt.IsNil)
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared-" + strconv.Itoa(i), Representation: protocol.ValueInput_DESIRED, Value: value}}})
		c.Assert(err, qt.IsNil)
		desired.Records = append(desired.Records, &protocol.ComparisonRecord{Kind: object.kind(), Subject: externalSubject(object.kind(), "", object.name), Value: normalized.Values[0]})
	}
	for _, object := range read {
		value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: object.kind(), Database: "/local", Schema: "app", Name: object.name}, readExternal(c, object))
		c.Assert(err, qt.IsNil)
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "read-" + object.name, Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		current.Records = append(current.Records, &protocol.ComparisonRecord{Kind: object.kind(), Subject: externalSubject(object.kind(), "/local", object.name), Value: normalized.Values[0]})
	}
	return &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: externalcompare.ServiceID, Kinds: externalcompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities, Desired: desired, Current: current}
}

// withSources is a 26.2 target with EnableExternalDataSources on.
func withSources() map[string]bool {
	return map[string]bool{"transactional_ddl": false, "external_data_sources": true, "external_data_source_secret_paths": true}
}

// withReplace is withSources with EnableReplaceIfExistsForExternalEntities on.
func withReplace() map[string]bool {
	capabilities := withSources()
	capabilities["external_object_replace"] = true
	return capabilities
}

// External objects compare as the host compares them, and a data source the
// plan drops and creates again takes the declared table over it along.
func TestExternalComparison_HappyPath(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	events, wider := external{name: "events", table: eventsOver("app/bucket")}, external{name: "events", table: eventsOver("app/bucket", "note")}
	for _, test := range []struct {
		name         string
		declared     []external
		read         []external
		capabilities map[string]bool
		changes      []string
		impact       protocol.ChangeSemantics_Impact
		access       protocol.ChangeSemantics_Access
	}{
		{name: "the same objects", declared: []external{bucket, events}, read: []external{bucket, events}, capabilities: withSources()},
		{name: "a creation", declared: []external{bucket, events}, capabilities: withSources(), changes: []string{"bucket", "events"},
			impact: protocol.ChangeSemantics_ADDITIVE, access: protocol.ChangeSemantics_UNCHANGED},
		{name: "a removal", read: []external{bucket, events}, capabilities: withSources(), changes: []string{"bucket", "events"},
			impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_NARROWS},
		{name: "a moved data source takes the table along", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withSources(),
			changes: []string{"bucket", "events"}, impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_UNCHANGED},
		{name: "a data source replaced in place keeps the table", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withReplace(),
			changes: []string{"bucket"}, impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_UNCHANGED},
		{name: "a new column", declared: []external{bucket, wider}, read: []external{bucket, events}, capabilities: withSources(),
			changes: []string{"events"}, impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_UNCHANGED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := externalRequest(c, test.declared, test.read, test.capabilities)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			var changed []string
			for _, change := range result.Changes {
				changed = append(changed, change.Subject.Name.Source)
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, test.impact)
				c.Assert(change.Value.ChangeSemantics.Access, qt.Equals, test.access)
				c.Assert(change.Value.ChangeSemantics.GetRemovesState(), qt.IsFalse)
				normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: change.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: change.Value.Value}}})
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(normalized.Values[0], change.Value), qt.IsTrue)
				_, _, database, err := schema.ExternalOperands(change.Value.Value)
				c.Assert(err, qt.IsNil)
				c.Assert(database, qt.Equals, "/local")
			}
			c.Assert(changed, qt.DeepEquals, test.changes)
		})
	}
}

// A target without external data sources leaves an object its read found
// unmanaged, as the host's reader leaves it unread, rather than dropping it.
func TestExternalComparisonLeavesAnObjectOfATargetWithoutTheCapability_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := externalRequest(c, nil, []external{{name: "bucket", source: bucketAt("https://a/")}}, map[string]bool{"transactional_ddl": false})
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Reason, qt.Equals, "The current external data source or its absence was not established: "+
		"target capability external_data_sources is unavailable, so Ptah leaves the object unmanaged")
}

// An object the desired source does not describe is kept as YDB holds it.
func TestExternalComparisonKeepsAnUndescribedObject_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := externalRequest(c, nil, []external{{name: "bucket", source: bucketAt("https://a/")}}, withSources())
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Desired.Records, qt.HasLen, 1)
	kept, err := schema.DesiredExternalSource(result.Desired.Records[0].Value.Value)
	c.Assert(err, qt.IsNil)
	spec, err := externals.SourceFromIntent(kept, "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(spec, qt.DeepEquals, *bucketAt("https://a/"))
}

// A plan that drops a data source a declared table reads, and a declaration
// on a target without the capability, are refused with the host's words.
func TestExternalComparison_FailurePath(t *testing.T) {
	bucket, events := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "events", table: eventsOver("app/bucket")}
	for _, test := range []struct {
		name         string
		declared     []external
		read         []external
		capabilities map[string]bool
		want         string
	}{
		{name: "a table over a dropped data source", declared: []external{events}, read: []external{bucket, events}, capabilities: withSources(),
			want: "external table app/events reads data source app/bucket, which the plan drops; declare the data source or move the table to one the plan keeps"},
		{name: "a declaration on a target without external data sources", declared: []external{bucket}, capabilities: map[string]bool{"transactional_ddl": false},
			want: "external data source app/bucket, which requires target capability external_data_sources, unavailable on this ydb target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := backend.New().CompareModels(t.Context(), externalRequest(c, test.declared, test.read, test.capabilities))
			c.Assert(err, qt.ErrorMatches, ".*"+test.want)
			c.Assert(result, qt.IsNil)
		})
	}
}

// externalPlan compares the objects on a target holding compared and plans
// every change that follows on a target holding capabilities.
func externalPlan(c *qt.C, declared, read []external, comparedWith, capabilities map[string]bool) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := externalRequest(c, declared, read, comparedWith)
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: externalplan.ServiceID, Identifiers: request.Identifiers, Capabilities: capabilities}
	for _, change := range compared.Changes {
		planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
	}
	plan, err := backend.New().PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	return planning, plan
}

// stepSummary names a planned step by its object, its operation and its
// first effect, in plan order.
type stepSummary struct {
	Object, Operation string
	Action            protocol.PlanningEffect_Action
}

// The batch plans each statement as one native step of the dependent phase,
// ordered as the host orders them: tables dropped before the data sources
// they read and created after. Each operation renders as a native unit and
// normalizes back to itself.
func TestExternalPlanning_HappyPath(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	events := external{name: "events", table: eventsOver("app/bucket")}
	for _, test := range []struct {
		name         string
		declared     []external
		read         []external
		capabilities map[string]bool
		steps        []stepSummary
		strategies   []string
		edges        [][2]int
	}{
		{name: "a creation", declared: []external{bucket, events}, capabilities: withSources(),
			steps:      []stepSummary{{"bucket", schema.ExternalCreate, protocol.PlanningEffect_CREATE}, {"events", schema.ExternalCreate, protocol.PlanningEffect_CREATE}},
			strategies: []string{"create the data source", "create the external table"}, edges: [][2]int{{0, 1}}},
		{name: "a removal", read: []external{bucket, events}, capabilities: withSources(),
			steps:      []stepSummary{{"events", schema.ExternalDrop, protocol.PlanningEffect_DROP}, {"bucket", schema.ExternalDrop, protocol.PlanningEffect_DROP}},
			strategies: []string{"drop the data source; the system it reads is untouched", "drop the external table; the files it reads stay"}, edges: [][2]int{{0, 1}}},
		{name: "a moved data source", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withSources(),
			steps: []stepSummary{{"events", schema.ExternalDrop, protocol.PlanningEffect_DROP}, {"bucket", schema.ExternalRecreate, protocol.PlanningEffect_ALTER},
				{"events", schema.ExternalRestore, protocol.PlanningEffect_CREATE}},
			strategies: []string{"drop the data source and create it again, with the external tables over it", "drop the external table and create it again"},
			edges:      [][2]int{{0, 1}, {0, 2}, {1, 2}}},
		{name: "a data source replaced in place", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withReplace(),
			steps: []stepSummary{{"bucket", schema.ExternalReplace, protocol.PlanningEffect_ALTER}}, strategies: []string{"replace the data source in place with CREATE OR REPLACE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			planning, plan := externalPlan(c, test.declared, test.read, test.capabilities, test.capabilities)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			var steps []stepSummary
			index := map[string]int{}
			for i, step := range plan.Contributions[0].Steps {
				index[step.Id.Name] = i
				c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
				c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
				_, operation, err := schema.ExternalOperationChange(step.Operation.Value.Value)
				c.Assert(err, qt.IsNil)
				steps = append(steps, stepSummary{step.Operation.Value.Value.Owner.Name, operation, step.Metadata.Effects[0].Action})
				normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
			}
			c.Assert(steps, qt.DeepEquals, test.steps)
			var strategies []string
			for _, receipt := range plan.Changes {
				strategies = append(strategies, receipt.Strategy)
			}
			c.Assert(strategies, qt.DeepEquals, test.strategies)
			var edges [][2]int
			for _, edge := range plan.Contributions[0].Dependencies {
				edges = append(edges, [2]int{index[edge.Before.Name], index[edge.After.Name]})
			}
			c.Assert(edges, qt.DeepEquals, test.edges)
			rendered, err := backend.New().Render(t.Context(), testplanning.RenderRequest(plan, test.capabilities))
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, len(test.steps))
		})
	}
}

// A statement the target cannot take refuses the batch with the host's
// words and plans nothing.
func TestExternalPlanning_FailurePath(t *testing.T) {
	pg := &ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "BASIC",
		Options: map[string]string{"LOGIN": "reader", "PASSWORD_SECRET_PATH": "/other/pw"}}
	for _, test := range []struct {
		name         string
		declared     []external
		read         []external
		compared     map[string]bool
		capabilities map[string]bool
		want         string
	}{
		{name: "a table over a source that is not object storage", declared: []external{{name: "pg", source: &ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "NONE"}},
			{name: "events", table: eventsOver("app/pg")}}, compared: withSources(), capabilities: withSources(),
			want: "external table app/events reads data source app/pg, a PostgreSQL source; an external table reads files, from an ObjectStorage source \\(`Only ObjectStorage source type supported`\\)"},
		{name: "two objects at one path", declared: []external{{name: "events", source: bucketAt("https://a/")}, {name: "events", table: eventsOver("app/bucket")}}, compared: withSources(), capabilities: withSources(),
			want: "external table app/events has the path of external data source app/events, and YDB keeps one object at a path \\(`unexpected path type`\\)"},
		{name: "a secret outside the database", declared: []external{{name: "pg", source: pg}}, compared: withSources(), capabilities: withSources(),
			want: `external data source app/pg: option PASSWORD_SECRET_PATH names secret "/other/pw", which is outside the database /local, so the data source cannot read it`},
		{name: "a drop on a target without external data sources", read: []external{{name: "events", table: eventsOver("app/bucket")}}, compared: withSources(), capabilities: map[string]bool{"transactional_ddl": false},
			want: "DROP EXTERNAL TABLE app/events, which requires target capability external_data_sources, unavailable on this ydb target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, plan := externalPlan(c, test.declared, test.read, test.compared, test.capabilities)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.Not(qt.HasLen), 0)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
		})
	}
}

// An operation whose operands cannot be carried out the way it names is
// refused by its normalization.
func TestExternalOperation_FailurePath(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.ExternalTableProperties, Schema: "app", Name: "events"}
	intent, err := externals.TableIntent(*eventsOver("app/bucket"))
	c.Assert(err, qt.IsNil)
	value, err := schema.CaptureExternalIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureExternalChange(schema.ExternalTableProperties, "change", owner, "/local", nil, value)
	c.Assert(err, qt.IsNil)
	for _, operation := range []string{schema.ExternalDrop, schema.ExternalReplace, schema.ExternalRestore, schema.ExternalRecreate} {
		got, err := schema.ExternalOperation("op", change.Value, operation)
		c.Assert(err, qt.ErrorMatches, "external table operation .*", qt.Commentf("%s", operation))
		c.Assert(got, qt.IsNil)
	}
	_, err = schema.CaptureExternalChange(schema.ExternalTableProperties, "change", owner, "local", nil, value)
	c.Assert(err, qt.ErrorMatches, "external table change names an invalid database")
}

// An external object operation renders in the TEXT form as the statement the
// host's embedded engine writes for it, with the paths as the declaration
// writes them.
func TestExternalRendersText(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	events := external{name: "events", table: eventsOver("app/bucket")}
	creation := []string{"CREATE EXTERNAL DATA SOURCE `app/bucket` WITH (\n    SOURCE_TYPE = 'ObjectStorage',\n    LOCATION = 'https://a/',\n    AUTH_METHOD = 'NONE'\n);",
		"CREATE EXTERNAL TABLE `app/events` (\n    `id` Int64 NOT NULL\n) WITH (\n    DATA_SOURCE = 'app/bucket',\n    LOCATION = '2026/',\n    CSV_DELIMITER = ';',\n    FORMAT = 'csv_with_names'\n);"}
	for _, test := range []struct {
		name         string
		declared     []external
		read         []external
		capabilities map[string]bool
		want         []string
	}{
		{name: "a creation", declared: []external{bucket, events}, capabilities: withSources(), want: creation},
		{name: "a removal", read: []external{bucket, events}, capabilities: withSources(), want: []string{"DROP EXTERNAL TABLE `app/events`;", "DROP EXTERNAL DATA SOURCE `app/bucket`;"}},
		{name: "a replacement", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withReplace(),
			want: []string{"CREATE OR REPLACE EXTERNAL DATA SOURCE `app/bucket` WITH (\n    SOURCE_TYPE = 'ObjectStorage',\n    LOCATION = 'https://b/',\n    AUTH_METHOD = 'NONE'\n);"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, plan := externalPlan(c, test.declared, test.read, test.capabilities, test.capabilities)
			rendering := testplanning.RenderRequest(plan, test.capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
			var statements []string
			for _, unit := range rendered.Units {
				statements = append(statements, unit.GetSql())
			}
			c.Assert(statements, qt.DeepEquals, test.want)
		})
	}
}

// An object dropped and created again gets the old one's owner and access
// entries from a native call, so the batch has no TEXT form.
func TestExternalRendersText_FailurePath(t *testing.T) {
	c := qt.New(t)
	_, plan := externalPlan(c, []external{{name: "bucket", source: bucketAt("https://b/")}, {name: "events", table: eventsOver("app/bucket")}},
		[]external{{name: "bucket", source: bucketAt("https://a/")}, {name: "events", table: eventsOver("app/bucket")}}, withSources(), withSources())
	rendering := testplanning.RenderRequest(plan, withSources())
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 0)
	c.Assert(rendered.Refusal.Reason, qt.Equals, protocol.RenderRefusal_NO_TEXT_FORM)
	c.Assert(rendered.Refusal.Message, qt.Matches, ".*an external object dropped and created again is given the owner and the access entries of the old one by a native call")
}
