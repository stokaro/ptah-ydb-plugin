package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// storageFacet captures the column storage of app.records in either
// representation.
func storageFacet(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.ComparisonRecord {
	c.Helper()
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.ColumnStorageProperties {
				model = candidate
			}
		}
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "storage", Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ColumnStorageProperties,
			SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner(), Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.ColumnStorageProperties, Subject: tableSubject(), Value: values.Values[0]}
}

// columnDeclaration declares app.records as a column table keyed by (ts, id)
// with the given storage and further facets.
func columnDeclaration(c *qt.C, storage string, records ...*protocol.ComparisonRecord) *protocol.DeclaredTable {
	c.Helper()
	table := &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"ts", "id"},
		Columns: []*protocol.DeclaredColumn{{Name: "ts", Type: "Timestamp", Primary: true}, {Name: "id", Type: "Uint64", Primary: true},
			{Name: "count", Type: "Uint64", Nullable: true}, {Name: "body", Type: "Utf8", Nullable: true}},
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
				Records: append([]*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_DESIRED, storage)}, records...)}}}
	return table
}

const (
	tiered      = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/ext"},{"applyAfterSeconds":"604800","delete":{}}]}}`
	deleteDaily = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}`
)

func TestCompileColumnTable_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, storage, want string }{
		{"the server's choices", `{}`,
			`{"columns":[{"name":"ts","type":{"typeId":"TIMESTAMP"},"notNull":true},{"name":"id","type":{"typeId":"UINT64"},"notNull":true},` +
				`{"name":"count","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false},{"name":"body","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],` +
				`"primaryKey":["ts","id"]}`},
		{"a hash key, a shard count and a tiered TTL", `{"hashColumns":["id"],"partitions":"3","ttl":` + tiered + `}`,
			`{"columns":[{"name":"ts","type":{"typeId":"TIMESTAMP"},"notNull":true},{"name":"id","type":{"typeId":"UINT64"},"notNull":true},` +
				`{"name":"count","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false},{"name":"body","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],` +
				`"hashColumns":["id"],"partitions":"3","primaryKey":["ts","id"],"ttl":` + tiered + `}`},
		{"a disabled TTL is none", `{"ttl":{"disabled":{}}}`,
			`{"columns":[{"name":"ts","type":{"typeId":"TIMESTAMP"},"notNull":true},{"name":"id","type":{"typeId":"UINT64"},"notNull":true},` +
				`{"name":"count","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false},{"name":"body","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],` +
				`"primaryKey":["ts","id"]}`},
		{"the data channels the server uses", `{"dataChannelCount":"64"}`,
			`{"columns":[{"name":"ts","type":{"typeId":"TIMESTAMP"},"notNull":true},{"name":"id","type":{"typeId":"UINT64"},"notNull":true},` +
				`{"name":"count","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false},{"name":"body","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],` +
				`"primaryKey":["ts","id"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, test.storage))
			c.Assert(err, qt.IsNil)
			encoded, err := protojson.Marshal(created)
			c.Assert(err, qt.IsNil)
			c.Assert(jsonValue(c, string(encoded)), qt.DeepEquals, jsonValue(c, test.want))
		})
	}
}

// TestCompileColumnTableWritesTheKeyNotNull compiles the key a Go source
// declares, whose fields marked primary are nullable unless they say not_null,
// into NOT NULL columns, as the host's embedded engine writes them, and leaves
// the declaration as it was.
func TestCompileColumnTableWritesTheKeyNotNull(t *testing.T) {
	c := qt.New(t)
	table := columnDeclaration(c, `{}`)
	table.Columns[0].Nullable, table.Columns[1].Nullable = true, true
	declared := proto.CloneOf(table)
	created, err := tablecreate.CompileColumnTable(owner(), table)
	c.Assert(err, qt.IsNil)
	encoded, err := protojson.Marshal(created)
	c.Assert(err, qt.IsNil)
	c.Assert(jsonValue(c, string(encoded)), qt.DeepEquals, jsonValue(c,
		`{"columns":[{"name":"ts","type":{"typeId":"TIMESTAMP"},"notNull":true},{"name":"id","type":{"typeId":"UINT64"},"notNull":true},`+
			`{"name":"count","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false},{"name":"body","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],`+
			`"primaryKey":["ts","id"]}`))
	c.Assert(proto.Equal(table, declared), qt.IsTrue)
}

// keep leaves a declaration as it is.
func keep(*qt.C, *protocol.DeclaredTable) {}

func TestCompileColumnTable_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		storage string
		modify  func(*qt.C, *protocol.DeclaredTable)
		wantErr string
	}{
		{name: "a hash column outside the key", storage: `{"hashColumns":["body"]}`, modify: keep, wantErr: `hash column "body" must belong to the primary key`},
		{name: "a TTL column after the first key column", storage: `{"ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`, modify: keep,
			wantErr: `TTL column "id" must be first in the primary key or have a LOCAL min_max index`},
		{name: "a TTL column the table does not declare", storage: `{"ttl":{"enabled":{"column":"gone","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`, modify: keep,
			wantErr: `the TTL reads column "gone", which the table does not declare`},
		{name: "a unit on a date column", storage: `{"ttl":{"enabled":{"column":"ts","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`, modify: keep,
			wantErr: `the TTL column "ts" is Timestamp, a date type, and YDB takes no unit for one`},
		{name: "a data channel count of its own", storage: `{"dataChannelCount":"32"}`, modify: keep, wantErr: `a column table's data channel count is set by the server.*`},
		{name: "local indexes", storage: `{"localIndexes":[{"name":"mm","columns":["count"],"storageId":"__LOCAL_METADATA","minMax":{}}]}`, modify: keep,
			wantErr: `the local indexes of a column table are not created by the provider yet`},
		{name: "row-table settings beside the storage", storage: `{}`, modify: func(c *qt.C, table *protocol.DeclaredTable) {
			table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.TableProperties, "", `{"keyBloomFilter":"ENABLED"}`))
		}, wantErr: `row-table partitioning, column families and changefeeds cannot be applied to a column table`},
		{name: "a column in a family", storage: `{}`, modify: func(c *qt.C, table *protocol.DeclaredTable) {
			table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.ColumnProperties, "body", `{"family":"cold"}`))
		}, wantErr: `row-table partitioning, column families and changefeeds cannot be applied to a column table`},
		{name: "a column default", storage: `{}`, modify: func(c *qt.C, table *protocol.DeclaredTable) {
			table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.ColumnProperties, "count",
				`{"literal":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"7"}}}`))
		}, wantErr: `column tables do not support defaults or Serial columns`},
		{name: "no key", storage: `{}`, modify: func(_ *qt.C, table *protocol.DeclaredTable) { table.PrimaryKey = nil },
			wantErr: `YDB table creation requires an ordered primary key`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			table := columnDeclaration(c, test.storage)
			test.modify(c, table)
			created, err := tablecreate.CompileColumnTable(owner(), table)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(created, qt.IsNil)
		})
	}
}

func TestColumnTableStatement(t *testing.T) {
	for _, test := range []struct{ name, storage, want string }{
		{"the server's choices", `{}`, "CREATE TABLE `/local/app/records` (\n    `ts` Timestamp NOT NULL,\n    `id` Uint64 NOT NULL,\n    `count` Uint64,\n    `body` Utf8,\n" +
			"    PRIMARY KEY (`ts`, `id`)\n) WITH (STORE = COLUMN);"},
		{"every setting", `{"hashColumns":["id"],"partitions":"3","ttl":` + tiered + `}`,
			"CREATE TABLE `/local/app/records` (\n    `ts` Timestamp NOT NULL,\n    `id` Uint64 NOT NULL,\n    `count` Uint64,\n    `body` Utf8,\n" +
				"    PRIMARY KEY (`ts`, `id`)\n) PARTITION BY HASH (`id`) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, " +
				"TTL = Interval(\"PT86400S\") TO EXTERNAL DATA SOURCE `/local/ext`, Interval(\"PT604800S\") DELETE ON `ts`);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, test.storage))
			c.Assert(err, qt.IsNil)
			c.Assert(tablecreate.ColumnTableStatement("/local/app/records", created), qt.Equals, test.want)
		})
	}
}

// TestColumnTableStatement_EpochTTLAndQuoting writes an epoch unit after AS and
// escapes a backtick and a backslash in a name.
func TestColumnTableStatement_EpochTTLAndQuoting(t *testing.T) {
	c := qt.New(t)
	table := columnDeclaration(c, `{"ttl":{"enabled":{"column":"n`+"`"+`s","columnUnit":"2","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`)
	table.PrimaryKey = []string{"n`s", "id"}
	table.Columns[0] = &protocol.DeclaredColumn{Name: "n`s", Type: "Uint64", Primary: true}
	created, err := tablecreate.CompileColumnTable(owner(), table)
	c.Assert(err, qt.IsNil)
	c.Assert(tablecreate.ColumnTableStatement(`/local/app\records`, created), qt.Equals, "CREATE TABLE `/local/app\\\\records` (\n    `n\\`s` Uint64 NOT NULL,\n"+
		"    `id` Uint64 NOT NULL,\n    `count` Uint64,\n    `body` Utf8,\n    PRIMARY KEY (`n\\`s`, `id`)\n) WITH (STORE = COLUMN, TTL = Interval(\"PT60S\") DELETE ON `n\\`s` AS MILLISECONDS);")
}

// columnCreationRequest plans the CREATE of app.records as a column table.
func columnCreationRequest(c *qt.C, storage string, capabilities map[string]bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	subject := tableSubject()
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, DatabasePath: "/local", ParentKinds: tablecreate.ParentKinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities,
		Tables: []*protocol.PlanningTable{{Subject: subject, Action: protocol.PlanningTable_CREATE, Desired: columnDeclaration(c, storage)}},
		CommonSteps: []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: proto.CloneOf(subject),
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}}}}
}

func TestColumnTablePlan_Create(t *testing.T) {
	c := qt.New(t)
	service := backend.New()
	request := columnCreationRequest(c, `{"hashColumns":["id"],"partitions":"3","ttl":`+deleteDaily+`}`, map[string]bool{"transactional_ddl": false})
	plan, err := service.PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	c.Assert(plan.Parents, qt.HasLen, 5)
	c.Assert(plan.Parents[0].Receipt.Strategy, qt.Matches, `Create the complete captured column table with YQL's CREATE TABLE.*`)
	step := plan.Contributions[0].Steps[0]
	c.Assert(step.Id.Name, qt.Equals, "column-table-create/000000")
	c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.ColumnOperationKind)
	c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
	c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_ADDITIVE)
	c.Assert(step.Metadata.Effects, qt.HasLen, 2)
	c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.scheme-path")
	normalized, err := service.NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: step.Operation.Value.Id, Representation: protocol.ValueInput_OPERATION, Value: step.Operation.Value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], step.Operation.Value), qt.IsTrue)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendered, err := service.Render(t.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(proto.Equal(rendered.Units[0].GetNative(), step.Operation.Value.Value), qt.IsTrue)
	c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
}

// TestColumnTablePlan_TieredTTLNeedsTheCapability refuses a TTL tier that
// moves rows unless the session established tiered_ttl, which it reads from
// the cluster's feature flags.
func TestColumnTablePlan_TieredTTLNeedsTheCapability(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities map[string]bool
		refused      bool
	}{
		{name: "established", capabilities: map[string]bool{"transactional_ddl": false, "tiered_ttl": true}},
		{name: "refused by the flags", capabilities: map[string]bool{"transactional_ddl": false, "tiered_ttl": false}, refused: true},
		{name: "not established", capabilities: map[string]bool{"transactional_ddl": false}, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(t.Context(), columnCreationRequest(c, `{"ttl":`+tiered+`}`, test.capabilities))
			c.Assert(err, qt.IsNil)
			c.Assert(len(plan.Diagnostics) == 1, qt.Equals, test.refused)
			c.Assert(len(plan.Contributions) == 0, qt.Equals, test.refused)
		})
	}
	c := qt.New(t)
	plan, err := backend.New().PlanFeatures(t.Context(), columnCreationRequest(c, `{"ttl":`+tiered+`}`, map[string]bool{"transactional_ddl": false}))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `column table "app/records" declares a TTL tier that moves rows to an external data source, `+
		`which requires target capability tiered_ttl, unavailable on this ydb target .*`)
}

// columnAlterRequest plans the ALTER of app.records with the given facets on
// both sides and the given common steps.
func columnAlterRequest(c *qt.C, desired, held []*protocol.ComparisonRecord, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	c.Helper()
	declared := columnDeclaration(c, `{}`)
	declared.Features.Facets.Records = desired
	current := &protocol.ObservedTable{Name: "records", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "ts"}, {Name: "id"}, {Name: "count"}, {Name: "body"}},
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
			Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}, Records: held}}}
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, DatabasePath: "/local", ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false}, CommonSteps: steps,
		Tables: []*protocol.PlanningTable{{Subject: tableSubject(), Action: protocol.PlanningTable_ALTER, Desired: declared, Current: current}}}
}

const heldStorage = `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":{"enabled":{"column":"ts","expireAfterSeconds":"86400","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`

// TestColumnTablePlan_AlterWithNoChange accounts for a column table that
// already holds the declared storage with no step.
func TestColumnTablePlan_AlterWithNoChange(t *testing.T) {
	for _, test := range []struct{ name, desired string }{
		{"the same storage", `{"hashColumns":["id"],"partitions":"3","ttl":` + deleteDaily + `}`},
		{"the server's choices", `{"ttl":` + deleteDaily + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnAlterRequest(c, []*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_DESIRED, test.desired)},
				[]*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, heldStorage)})
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 5)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
			c.Assert(plan.Parents[0].Receipt.Strategy, qt.Matches, `The column table already holds the declared storage.*`)
		})
	}
}

// TestColumnTablePlan_AlterRefusals refuses each change no statement makes in
// place, with the reason, and returns no partial plan.
func TestColumnTablePlan_AlterRefusals(t *testing.T) {
	desired := func(c *qt.C, payload string) []*protocol.ComparisonRecord {
		return []*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_DESIRED, payload)}
	}
	held := func(c *qt.C) []*protocol.ComparisonRecord {
		return []*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, heldStorage)}
	}
	for _, test := range []struct {
		name    string
		request func(*qt.C) *protocol.PlanFeaturesRequest
		want    string
	}{
		{"another shard count", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, desired(c, `{"partitions":"4","ttl":`+deleteDaily+`}`), held(c))
		}, `table "app/records": changing a column table's hash key or shard count requires an explicit data migration \(the table has 3 shards, and the declaration asks for 4\)`},
		{"another hash key", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, desired(c, `{"hashColumns":["ts","id"],"ttl":`+deleteDaily+`}`), held(c))
		}, `table "app/records": changing a column table's hash key or shard count requires an explicit data migration \(the table is hashed by \["id"\], and the declaration asks for \["ts" "id"\]\)`},
		{"local indexes the declaration states otherwise", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, desired(c, `{"ttl":`+deleteDaily+`,"localIndexes":[{"name":"mm","columns":["ts"],"storageId":"__LOCAL_METADATA","minMax":{}}]}`), held(c))
		}, `table "app/records": the local indexes of a column table are not added or dropped by the provider yet`},
		{"a row table declared as a column table", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, desired(c, `{}`), []*protocol.ComparisonRecord{observedFacet(c, schema.TableProperties, "", `{"primaryKey":["ts","id"]}`)})
		}, `table "app/records": changing between row and column storage requires an explicit data migration`},
		{"a column table declared with row-table settings", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, []*protocol.ComparisonRecord{facet(c, schema.TableProperties, "", `{"keyBloomFilter":"ENABLED"}`)}, held(c))
		}, `table "app/records": changing between row and column storage requires an explicit data migration`},
		{"a column table a complete source declares as a row table", func(c *qt.C) *protocol.PlanFeaturesRequest {
			request := columnAlterRequest(c, nil, held(c))
			request.Tables[0].Desired.Features.Facets.Coverage.Kinds = []*protocol.ComparisonKindCoverage{{Model: schema.NativeDesiredModel(schema.ColumnStorageProperties),
				Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}}}
			return request
		}, `table "app/records": changing between row and column storage requires an explicit data migration`},
		{"a storage the read did not capture", func(c *qt.C) *protocol.PlanFeaturesRequest {
			return columnAlterRequest(c, desired(c, `{}`), nil)
		}, `The table's current storage was not captured`},
		{"a column step", func(c *qt.C) *protocol.PlanFeaturesRequest {
			step := commonStep("add", tableSubject(), "note", &protocol.PlanningEffect{Subject: tableSubject(), Action: protocol.PlanningEffect_READ})
			return columnAlterRequest(c, desired(c, `{"ttl":`+deleteDaily+`}`), held(c), step)
		}, `A common step is outside the supported column table lifecycle: .*`},
		{"a rebuild", func(c *qt.C) *protocol.PlanFeaturesRequest {
			request := columnAlterRequest(c, desired(c, `{"ttl":`+deleteDaily+`}`), held(c))
			request.Tables[0].Action = protocol.PlanningTable_REBUILD
			return request
		}, `table "app/records": column-table rebuilds require an explicit data migration`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(t.Context(), test.request(c))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// TestColumnTablePlan_AlterKeepsAStorageTheSourceCannotDescribe leaves a column
// table alone when the source neither declares its storage nor describes it.
func TestColumnTablePlan_AlterKeepsAStorageTheSourceCannotDescribe(t *testing.T) {
	c := qt.New(t)
	plan, err := backend.New().PlanFeatures(t.Context(), columnAlterRequest(c, nil, []*protocol.ComparisonRecord{storageFacet(c, protocol.ValueInput_OBSERVED, heldStorage)}))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}

func TestColumnTableOperation_FailurePath(t *testing.T) {
	model := tablecreate.ColumnModel()
	for _, test := range []struct{ name, payload string }{
		{"a nullable key column", `{"columns":[{"name":"id","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false}],"primaryKey":["id"]}`},
		{"a hash column outside the key", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},{"name":"b","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"hashColumns":["b"]}`},
		{"a column default", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true,"fromLiteral":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"1"}}}],"primaryKey":["id"]}`},
		{"a TTL column after the first key column", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},{"name":"t","type":{"typeId":"TIMESTAMP"},"notNull":true}],` +
			`"primaryKey":["id","t"],"ttl":{"enabled":{"column":"t","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`},
		{"a disabled TTL", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"ttl":{"disabled":{}}}`},
		{"an unknown field", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"storeType":"STORE_TYPE_COLUMN"}`},
		{"a null setting", `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"hashColumns":null}`},
		{"no columns", `{"primaryKey":["id"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			normalized, err := tablecreate.NormalizeColumnTable(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.ColumnOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: model.Encoding,
				DefinitionSha256: model.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// TestColumnTablePreparationAndProjection derives a column table's key and
// projects no value beyond its declared storage.
func TestColumnTablePreparationAndProjection(t *testing.T) {
	c := qt.New(t)
	identifiers := &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}
	prepared, err := backend.New().PrepareTables(t.Context(), &protocol.PrepareTablesRequest{Engine: "ydb", Identifiers: identifiers,
		Capabilities: map[string]bool{"transactional_ddl": false}, Tables: []*protocol.TablePreparationInput{{Subject: tableSubject(), Desired: columnDeclaration(c, `{}`),
			CurrentKnowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_ABSENT}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, []string{"ts", "id"})
	projected, err := backend.New().ProjectTableCreations(t.Context(), &protocol.ProjectTableCreationsRequest{Engine: "ydb", Identifiers: identifiers,
		Capabilities: map[string]bool{"transactional_ddl": false}, Tables: []*protocol.TableCreationInput{{Subject: tableSubject(), Declaration: columnDeclaration(c, `{"partitions":"2"}`),
			FacetOwners: []*protocol.ComparisonIdentity{tableSubject()}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(projected.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, []string{"ts", "id"})
	c.Assert(projected.Tables[0].DesiredFacets, qt.HasLen, 0)
	c.Assert(projected.Tables[0].ObservedFacets, qt.HasLen, 0)
}
