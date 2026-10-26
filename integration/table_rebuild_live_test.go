//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// rebuildFixture is a directory with app.records(id Uint64, payload Utf8, n
// Int32) holding rows, and a provider session for a host that lists text
// units and the planning policy.
type rebuildFixture struct {
	client    *providerruntime.Client
	session   *protocol.Session
	opened    *connection.Connection
	directory string
	database  string
}

func newRebuildFixture(c *qt.C, columns, rows string) *rebuildFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "records")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` ("+columns+", PRIMARY KEY (id)) WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED)"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, name := range []string{"records", "__ptah_rebuild_records", "__ptah_replaced_records"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` "+rows)
	c.Assert(err, qt.IsNil)
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature, provider.PlanningPolicyFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &rebuildFixture{client: client, session: session, opened: opened, directory: directory, database: database}
}

// observed is records as a host hands it to planning: each column with its
// type, nullability and key membership, and the table's facets.
func (f *rebuildFixture) observed(c *qt.C) *protocol.ObservedTable {
	c.Helper()
	table := observedTable(c, f.client, f.session, f.directory)
	snapshot, err := f.client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: f.session.Id, Schemas: []string{f.directory}})
	c.Assert(err, qt.IsNil)
	for i, column := range snapshot.Tables[0].Columns {
		table.Columns[i].DataType, table.Columns[i].PrimaryKey = column.Type, column.PrimaryKey
		table.Columns[i].IsNullable = map[bool]string{true: "YES", false: "NO"}[column.Nullable]
	}
	return table
}

// plan plans the alteration of records into columns under policy.
func (f *rebuildFixture) plan(c *qt.C, columns []*protocol.DeclaredColumn, policy *protocol.PlanningPolicy) *protocol.PlannedFeatures {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: f.database, Schema: f.directory, Name: "records"}
	desired := &protocol.DeclaredTable{Name: "records", Schema: f.directory, PrimaryKey: []string{"id"}, Columns: columns,
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)}, Policy: policy,
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_ALTER, Desired: desired, Current: f.observed(c)}}}
	plan, err := f.client.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return plan
}

func (f *rebuildFixture) execute(c *qt.C, plan *protocol.PlannedFeatures, form protocol.RenderRequest_Form) (*protocol.Execution, error) {
	c.Helper()
	rendering := renderSteps(scheduled(c, plan), f.session.Capabilities)
	rendering.Form = form
	rendered, err := f.client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 5)
	return f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "rebuild", Units: rendered.Units})
}

// held is what records holds after a rebuild: each column with its type and
// nullability, the rows, and the tables of its directory.
func (f *rebuildFixture) held(c *qt.C) []any {
	c.Helper()
	snapshot, err := f.client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: f.session.Id, Schemas: []string{f.directory}})
	c.Assert(err, qt.IsNil)
	var columns []string
	for _, column := range snapshot.Tables[0].Columns {
		columns = append(columns, column.Name+" "+column.Type+map[bool]string{true: "?", false: ""}[column.Nullable])
	}
	rows, err := f.opened.DB.QueryContext(c.Context(), "SELECT CAST(id AS Utf8) || ':' || CAST(n AS Utf8) || ':' || payload FROM `"+
		path.Join(f.database, f.directory, "records")+"` ORDER BY id")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	var values []string
	for rows.Next() {
		var value string
		c.Assert(rows.Scan(&value), qt.IsNil)
		values = append(values, value)
	}
	c.Assert(rows.Err(), qt.IsNil)
	return []any{columns, values, childNames(c, f.opened, path.Join(f.database, f.directory))}
}

var rebuiltColumns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8"}, {Name: "n", Type: "Int64", Nullable: true}}

// TestTableRebuildSubprocess changes a column's type and makes another NOT
// NULL, which YDB makes only by rebuilding the table: refused without the
// policy's rebuild, and with it planned as five steps that leave the table
// with its rows, the new types and its automatic partitioning, by the native
// calls and by the statements a migration file holds alike.
func TestTableRebuildSubprocess(t *testing.T) {
	const rows = "(id, payload, n) VALUES (1ul, 'a'u, 5), (2ul, 'b'u, 7)"
	c := qt.New(t)
	refused := newRebuildFixture(c, "id Uint64, payload Utf8, n Int32", rows)
	plan := refused.plan(c, rebuiltColumns, &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"})
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `making column "payload" of table ".*/records" NOT NULL, .*when asked with --allow-table-rebuild`)

	var held [][]any
	for _, form := range []protocol.RenderRequest_Form{protocol.RenderRequest_ANY, protocol.RenderRequest_TEXT} {
		f := newRebuildFixture(c, "id Uint64, payload Utf8, n Int32", rows)
		plan := f.plan(c, rebuiltColumns, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
		c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
		result, err := f.execute(c, plan, form)
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		c.Assert(result.AppliedCount, qt.Equals, uint32(5))
		held = append(held, f.held(c))
		// The read-back is the declaration: nothing is left to rebuild.
		c.Assert(f.plan(c, rebuiltColumns, &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"}).Diagnostics, qt.HasLen, 0)
		// The new table keeps the setting the declaration leaves out.
		records := f.observed(c).Features.Facets.Records
		at := slices.IndexFunc(records, func(record *protocol.ComparisonRecord) bool { return record.Kind == schema.TableProperties })
		c.Assert(string(records[at].Value.Value.Payload), qt.Contains, `"partitioningByLoad":"ENABLED"`)
	}
	c.Assert(held[0], qt.DeepEquals, []any{[]string{"id Uint64", "payload Utf8", "n Int64?"}, []string{"1:5:a", "2:7:b"}, []string{"records"}})
	c.Assert(held[1], qt.DeepEquals, held[0])
}

// TestTableRebuildKeepsTheOldTableSubprocess rebuilds a table one of whose
// rows does not convert to the declared type: the copy fails whole, after the
// new table was created, and the old table keeps every row.
func TestTableRebuildKeepsTheOldTableSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRebuildFixture(c, "id Uint64, payload Utf8, n Utf8", "(id, payload, n) VALUES (1ul, 'a'u, '5'u), (2ul, 'b'u, 'x'u)")
	plan := f.plan(c, []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8", Nullable: true},
		{Name: "n", Type: "Int64", Nullable: true}}, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	result, err := f.execute(c, plan, protocol.RenderRequest_ANY)
	c.Assert(err, qt.ErrorAs, new(*provider.ExecutionError))
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	var count uint64
	c.Assert(f.opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+path.Join(f.database, f.directory, "records")+"`").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(2))
	listing := childNames(c, f.opened, path.Join(f.database, f.directory))
	slices.Sort(listing)
	c.Assert(strings.Join(listing, ","), qt.Equals, "__ptah_rebuild_records,records")
}
