//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// heldSettings are the settings of the fixture's table the provider reads
// back that a text unit and a native call can change: everything but the
// table's identity, its statistics and its partition bounds.
func (f *facetFixture) heldSettings(c *qt.C) map[string]any {
	c.Helper()
	var held map[string]any
	c.Assert(json.Unmarshal(f.heldTable(c), &held), qt.IsNil)
	for _, volatile := range []string{"self", "shardKeyBounds", "tableStats"} {
		delete(held, volatile)
	}
	return held
}

// alterationScenario changes the fixture's table and returns what the change
// left: the provider's read-back of what it touched.
type alterationScenario struct {
	name    string
	clauses string
	// apply makes the change through the fixture, in whatever form the
	// fixture renders, and returns the read-back to compare.
	apply func(c *qt.C, f *facetFixture) any
	// statements are the openings of the text units the change takes.
	statements []string
}

func alterationScenarios() []alterationScenario {
	declaredIndex := declaredIndexes(indexSpec("by_payload", "globalIndex", `["payload"]`, `["ts"]`))
	return []alterationScenario{
		{name: "a TTL set, changed and removed", statements: []string{"ALTER TABLE", "ALTER TABLE", "ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				var held []string
				for _, declared := range []string{ttlDeclaration(hourlyTTL), ttlDeclaration(dailyTTL), noTTLDeclaration} {
					f.apply(c, "ttl", declared, f.compare(c, declared).Changes)
					held = append(held, f.heldTTL(c))
					c.Assert(f.compare(c, declared).Changes, qt.HasLen, 0)
				}
				return held
			}},
		{name: "a family's compression", clauses: ", FAMILY archive (COMPRESSION = 'off')", statements: []string{"ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				declared := `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`
				f.apply(c, "family", declared, f.compare(c, declared).Changes)
				c.Assert(f.compare(c, declared).Changes, qt.HasLen, 0)
				return f.heldSettings(c)
			}},
		{name: "the automatic partitioning", statements: []string{"ALTER TABLE", "ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				var held []any
				for _, declared := range []string{partitioningDeclaration(`{"partitioningByLoad":"ENABLED","minPartitionsCount":"2"}`),
					partitioningDeclaration(`{"partitioningBySize":"DISABLED"}`)} {
					f.apply(c, "partitioning", declared, f.compare(c, declared).Changes)
					held = append(held, f.heldPartitioning(c))
					c.Assert(f.compare(c, declared).Changes, qt.HasLen, 0)
				}
				return held
			}},
		{name: "an index added", statements: []string{"ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				f.applyRecords(c, "index-add", f.compareIndexes(c, declaredIndex).Changes, desiredFacet(c, f.client, f.owner, schema.IndexProperties, declaredIndex))
				c.Assert(f.compareIndexes(c, declaredIndex).Changes, qt.HasLen, 0)
				held := f.heldIndexes(c)["by_payload"]
				return []any{held.GetIndexColumns(), held.GetDataColumns(), held.GetStatus().String()}
			}},
		{name: "an index dropped", clauses: ", INDEX by_payload GLOBAL SYNC ON (payload)", statements: []string{"ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				declared := declaredIndexes()
				f.applyRecords(c, "index-drop", f.compareIndexes(c, declared).Changes, desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
				return len(f.heldIndexes(c))
			}},
		{name: "a column added and an index on it", statements: []string{"ALTER TABLE", "ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				f.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true},
					{Name: "payload", Type: "Utf8", Nullable: true}, {Name: "label", Type: "Utf8", Nullable: true}}
				f.steps = []*protocol.CommonPlanningStep{f.columnStep("add-label", "label", true)}
				declared := declaredIndexes(indexSpec("by_label", "globalIndex", `["label"]`, ""))
				plan := f.plan(c, f.compareIndexes(c, declared).Changes, desiredFacet(c, f.client, f.owner, schema.TableProperties, noTTLDeclaration),
					desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
				c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
				c.Assert(f.execute(c, "column-and-index", plan).Outcome, qt.Equals, protocol.Execution_APPLIED)
				return []any{f.heldColumns(c), f.heldIndexes(c)["by_label"].GetIndexColumns()}
			}},
		{name: "an index and the column it reads dropped", clauses: ", INDEX by_payload GLOBAL SYNC ON (payload)", statements: []string{"ALTER TABLE", "ALTER TABLE"},
			apply: func(c *qt.C, f *facetFixture) any {
				f.columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}}
				f.steps = []*protocol.CommonPlanningStep{f.columnStep("drop-payload", "payload", false)}
				declared := declaredIndexes()
				plan := f.plan(c, f.compareIndexes(c, declared).Changes, desiredFacet(c, f.client, f.owner, schema.TableProperties, noTTLDeclaration),
					desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
				c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
				c.Assert(f.execute(c, "index-and-column", plan).Outcome, qt.Equals, protocol.Execution_APPLIED)
				return []any{f.heldColumns(c), len(f.heldIndexes(c))}
			}},
	}
}

// TestTextUnitsAlterAsTheNativeCallSubprocess makes each change of a row
// table twice, on twin tables: once through the native call and once through
// the YQL statements the TEXT form writes for a migration file. Both read back
// the same, and after either nothing is left to plan.
func TestTextUnitsAlterAsTheNativeCallSubprocess(t *testing.T) {
	for _, scenario := range alterationScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			c := qt.New(t)
			native := newFacetFixture(c, scenario.clauses, "")
			text := newFacetFixtureListing(c, scenario.clauses, "", provider.TextUnitsFeature)
			text.form = protocol.RenderRequest_TEXT
			c.Assert(scenario.apply(c, text), qt.DeepEquals, scenario.apply(c, native))
			var openings []string
			for _, unit := range text.rendered {
				openings = append(openings, unit.GetSql()[:len("ALTER TABLE")])
			}
			c.Assert(openings, qt.DeepEquals, scenario.statements)
		})
	}
}

// TestTextUnitsCreateAsTheNativeCallSubprocess creates one declaration twice:
// through the native call, and through the CREATE TABLE the TEXT form writes.
// The two tables read back the same.
func TestTextUnitsCreateAsTheNativeCallSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	client := startProviderFor(c, provider.TextUnitsFeature)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	create := func(c *qt.C, name string, form protocol.RenderRequest_Form) *protocol.RenderResult {
		c.Helper()
		c.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		})
		owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: name}
		declared := &protocol.DeclaredTable{Name: name, Schema: directory, PrimaryKey: []string{"second", "first"},
			Columns: []*protocol.DeclaredColumn{{Name: "first", Type: "Uint64", Primary: true}, {Name: "second", Type: "Utf8", Primary: true},
				{Name: "payload", Type: "String", Nullable: true}, {Name: "expires", Type: "Timestamp", Nullable: true}, {Name: "hits", Type: "Uint64"}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}, Records: []*protocol.ComparisonRecord{
					desiredFacet(c, client, owner, schema.TableProperties, `{"primaryKey":["second","first"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}],`+
						`"ttlSettings":{"dateTypeColumn":{"columnName":"expires","expireAfterSeconds":3600}},"keyBloomFilter":"ENABLED",`+
						`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"2","maxPartitionsCount":"4"}}`),
					desiredFacet(c, client, &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{name}, Name: "payload"},
						schema.ColumnProperties, `{"family":"archive"}`),
					desiredFacet(c, client, &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{name}, Name: "hits"},
						schema.ColumnProperties, `{"literal":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"7"}}}`),
				}}}}
		// The column facets belong to this table, not to the records table
		// desiredFacet names.
		for _, column := range declared.Features.Facets.Records[1:] {
			column.Subject.Parent = &protocol.ComparisonPart{Source: name, Normalized: name}
		}
		planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
			Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE, Desired: declared}}}
		prepareCapturedCreation(c, client, planning)
		projectCapturedCreation(c, client, planning)
		plan, err := client.PlanFeatures(ctx, planning)
		c.Assert(err, qt.IsNil)
		c.Assert(plan.Diagnostics, qt.HasLen, 0)
		rendering := testplanning.RenderRequest(plan, session.Capabilities)
		rendering.Form = form
		rendered, err := client.Render(ctx, rendering)
		c.Assert(err, qt.IsNil)
		c.Assert(rendered.Units, qt.HasLen, 1)
		result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "create-" + name, Units: rendered.Units})
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		return rendered
	}
	native := create(c, "by_call", protocol.RenderRequest_ANY)
	c.Assert(native.Units[0].GetNative(), qt.IsNotNil)
	text := create(c, "by_text", protocol.RenderRequest_TEXT)
	c.Assert(text.Units[0].GetSql(), qt.Equals, "CREATE TABLE `"+directory+"/by_text` (\n    `first` Uint64 NOT NULL,\n    `second` Utf8 NOT NULL,\n"+
		"    `payload` String FAMILY `archive`,\n    `expires` Timestamp,\n    `hits` Uint64 NOT NULL DEFAULT 7ul,\n    PRIMARY KEY (`second`, `first`),\n"+
		"    FAMILY `archive` (COMPRESSION = 'lz4')\n) WITH (TTL = Interval(\"PT1H\") ON `expires`, AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 4, KEY_BLOOM_FILTER = ENABLED);")
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 2)
	values, err := provider.InspectionValues(client.Description(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}, snapshot)
	c.Assert(err, qt.IsNil)
	held := map[string][]any{}
	for _, table := range snapshot.Tables {
		var columns []any
		for _, column := range table.Columns {
			columns = append(columns, []any{column.Name, column.Type, column.Nullable, column.PrimaryKey, column.GetDefaultExpression()})
		}
		held[table.Identity.Name] = append(held[table.Identity.Name], columns)
	}
	for _, input := range values.GetValues() {
		value := input.GetValue()
		var payload map[string]any
		c.Assert(json.Unmarshal(value.Payload, &payload), qt.IsNil)
		for _, volatile := range []string{"self", "shardKeyBounds", "tableStats"} {
			delete(payload, volatile)
		}
		table := facetTable(value.Owner)
		held[table] = append(held[table], []any{value.Kind, value.Owner.Kind, strings.TrimPrefix(value.Owner.Name, table), payload})
	}
	c.Assert(held["by_text"], qt.DeepEquals, held["by_call"])
	c.Assert(len(held["by_call"]) > 2, qt.IsTrue)
	var hits uint64
	_, err = opened.DB.ExecContext(ctx, "UPSERT INTO `"+path.Join(database, directory, "by_text")+"` (first, second) VALUES (1u, 'a'u)")
	c.Assert(err, qt.IsNil)
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT hits FROM `"+path.Join(database, directory, "by_text")+"`").Scan(&hits), qt.IsNil)
	c.Assert(hits, qt.Equals, uint64(7))
}

// facetTable is the table a facet's owner belongs to: the table itself, or
// the table of a column.
func facetTable(owner *protocol.Reference) string {
	if owner.GetKind() == "column" {
		return owner.GetPath()[0]
	}
	return owner.GetName()
}

// coordinationText is a coordination node declared, compared and planned
// through a provider that renders text units, in one directory of the test
// database.
type coordinationText struct {
	client    *providerruntime.Client
	session   *protocol.Session
	directory string
	database  string
	nodes     Ydb_Coordination_V1.CoordinationServiceClient
}

// plan compares the node a source declares in read consistency mode with
// the node the server holds, and renders the change in the TEXT form. An
// empty mode declares the node with its defaults, and drop declares none.
func (f *coordinationText) plan(c *qt.C, mode string, drop bool) *protocol.ComparedModels {
	c.Helper()
	nodes := map[string]any{}
	if !drop {
		fields := map[string]any{"schema": f.directory}
		if mode != "" {
			fields["read_consistency_mode"] = mode
		}
		nodes["node"] = fields
	}
	data, err := json.Marshal(map[string]any{"coordination_nodes": nodes})
	c.Assert(err, qt.IsNil)
	source, err := testsource.Request(string(data))
	c.Assert(err, qt.IsNil)
	decoded, err := f.client.DecodeSource(c.Context(), source)
	c.Assert(err, qt.IsNil)
	snapshot, err := f.client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: f.session.Id, Schemas: []string{f.directory}})
	c.Assert(err, qt.IsNil)
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: coordinationcompare.ServiceID, Kinds: []string{schema.CoordinationProperties},
		DatabasePath: f.database, Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Desired: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: capturedCoordinationCoverage(decoded.Documents[0].Coverage, f.database)},
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedCoordinationCoverage(snapshot.Coverage, f.database)}}
	for _, value := range decoded.Documents[0].Fragments[0].Values {
		owner := proto.CloneOf(value.Value.NormalizedOwner)
		owner.Database = f.database
		subject := testplanning.Subject(owner)
		subject.Database.Defaulted = true
		request.Desired.Records = append(request.Desired.Records, &protocol.ComparisonRecord{Kind: schema.CoordinationProperties, Subject: subject, Value: value.Value})
	}
	for _, value := range snapshot.NativeObjects {
		if value.Kind != schema.CoordinationProperties {
			continue
		}
		normalized, err := f.client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "observed",
			Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: schema.CoordinationProperties, Subject: testplanning.Subject(value.Owner),
			Value: normalized.Values[0]})
	}
	result, err := f.client.CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return result
}

// text renders the one change of a comparison as its text unit.
func (f *coordinationText) text(c *qt.C, compared *protocol.ComparedModels) *protocol.Unit {
	c.Helper()
	c.Assert(compared.Changes, qt.HasLen, 1)
	change := compared.Changes[0]
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: coordinationplan.ServiceID, DatabasePath: f.database, Capabilities: f.session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Changes:     []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}}
	plan, err := f.client.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	rendering := testplanning.RenderRequest(plan, f.session.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := f.client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetSql(), qt.Not(qt.Equals), "")
	return rendered.Units[0]
}

// held is the configuration the server holds for the node.
func (f *coordinationText) held(c *qt.C) *Ydb_Coordination.Config {
	c.Helper()
	described, err := f.nodes.DescribeNode(c.Context(), &Ydb_Coordination.DescribeNodeRequest{Path: path.Join(f.database, f.directory, "node")})
	c.Assert(err, qt.IsNil)
	var result Ydb_Coordination.DescribeNodeResult
	c.Assert(described.GetOperation().GetResult().UnmarshalTo(&result), qt.IsNil)
	return result.GetConfig()
}

func (f *coordinationText) execute(c *qt.C, transaction string, units ...*protocol.Unit) (*protocol.Execution, error) {
	c.Helper()
	return f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, TransactionId: transaction, RequestId: "text", Units: units})
}

// TestCoordinationTextUnitsSubprocess creates, changes and drops a
// coordination node through the statements the TEXT form writes, which the
// provider runs through the coordination service, and reads each result
// back from the server.
func TestCoordinationTextUnitsSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	nodes := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = nodes.DropNode(cleanup, &Ydb_Coordination.DropNodeRequest{Path: path.Join(database, directory, "node")})
	})
	client := startProviderFor(c, provider.TextUnitsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	f := &coordinationText{client: client, session: session, directory: directory, database: database, nodes: nodes}

	create := f.text(c, f.plan(c, "strict", false))
	c.Assert(create.GetSql(), qt.Equals, "CREATE COORDINATION NODE `"+directory+"/node` WITH (read_consistency_mode = 'strict');")
	result, err := f.execute(c, "", create)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(f.held(c).GetReadConsistencyMode(), qt.Equals, Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT)
	c.Assert(f.plan(c, "strict", false).Changes, qt.HasLen, 0)

	// The service would answer a second creation with success and keep the
	// node as it is, so the statement is refused before it is sent.
	result, err = f.execute(c, "", create)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(result.Message, qt.Equals, "create YDB coordination node "+path.Join(database, directory, "node")+": the node already exists")

	// A change made outside the plan is kept where the statement names
	// nothing of it.
	changed, err := nodes.AlterNode(c.Context(), &Ydb_Coordination.AlterNodeRequest{Path: path.Join(database, directory, "node"),
		Config: &Ydb_Coordination.Config{SessionGracePeriodMillis: 12_000}})
	c.Assert(err, qt.IsNil)
	c.Assert(changed.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	alter := f.text(c, f.plan(c, "", false))
	c.Assert(alter.GetSql(), qt.Equals, "ALTER COORDINATION NODE `"+directory+"/node` SET (session_grace_period = Interval('PT10S'), read_consistency_mode = 'relaxed');")
	result, err = f.execute(c, "", alter)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(f.plan(c, "", false).Changes, qt.HasLen, 0)
	held := f.held(c)
	c.Assert(held.GetSessionGracePeriodMillis(), qt.Equals, uint32(10_000))
	c.Assert(held.GetReadConsistencyMode(), qt.Equals, Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED)

	// A coordination node statement runs outside a transaction, as YDB runs
	// every scheme statement.
	drop := f.text(c, f.plan(c, "", true))
	c.Assert(drop.GetSql(), qt.Equals, "DROP COORDINATION NODE `"+directory+"/node`;")
	handle, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "text",
		Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	result, err = f.execute(c, handle.Id, drop)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(result.DiagnosticCode, qt.Equals, "ydb.native-transaction-forbidden")
	_, err = client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: handle.Id,
		Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	result, err = f.execute(c, "", drop)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(f.plan(c, "", true).Changes, qt.HasLen, 0)
}

// TestCoordinationStatementsOfAMigrationFileSubprocess runs the statements a
// migration file the host's embedded engine wrote holds, read with the path
// prefix the file sets and in a dev realm, and reads the nodes back.
func TestCoordinationStatementsOfAMigrationFileSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	nodes := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	prefix := path.Join(database, directory)
	result := executeSQL(c, client, session.Id, "",
		"PRAGMA TablePathPrefix(\""+prefix+"\");\nCREATE COORDINATION NODE `limits` WITH (self_check_period = Interval('PT2S'), session_grace_period = Interval('PT15S'), "+
			"read_consistency_mode = 'strict', attach_consistency_mode = 'relaxed', rate_limiter_counters_mode = 'detailed');",
		"DROP COORDINATION NODE `"+directory+"/limits`;")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	c.Assert(result.AppliedCount, qt.Equals, uint32(2))
	listing := childNames(c, opened, prefix)
	c.Assert(slices.Contains(listing, "limits"), qt.IsFalse)

	// A relative path names a node inside the realm, as it names a table there.
	realm, root, _ := realmSession(c, client)
	result = executeSQL(c, client, realm, "", "CREATE COORDINATION NODE `in_realm` WITH (rate_limiter_counters_mode = 'detailed');")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	described, err := nodes.DescribeNode(c.Context(), &Ydb_Coordination.DescribeNodeRequest{Path: path.Join(root, "in_realm")})
	c.Assert(err, qt.IsNil)
	var node Ydb_Coordination.DescribeNodeResult
	c.Assert(described.GetOperation().GetResult().UnmarshalTo(&node), qt.IsNil)
	c.Assert(node.GetConfig().GetRateLimiterCountersMode(), qt.Equals, Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED)
	refused, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: realm, RequestId: "escape", Units: []*protocol.Unit{{ChangeId: "escape",
		Body: &protocol.Unit_Sql{Sql: "CREATE COORDINATION NODE `" + prefix + "/escaped`;"}}}})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(refused.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	result = executeSQL(c, client, realm, "", "DROP COORDINATION NODE `in_realm`;")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
}
