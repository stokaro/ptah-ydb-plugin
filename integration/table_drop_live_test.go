//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// dropFixture is a directory of the test database with one table in it, and
// a provider session that plans and runs its removal.
type dropFixture struct {
	client    *providerruntime.Client
	session   *protocol.Session
	opened    *connection.Connection
	directory string
	database  string
}

func newDropFixture(c *qt.C, address, definition string) *dropFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "records")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+absolute+"` "+definition)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &dropFixture{client: client, session: session, opened: opened, directory: directory, database: database}
}

// observed is the table as a host hands it to planning: the read's columns,
// with their key membership, and its facets.
func (f *dropFixture) observed(c *qt.C) *protocol.ObservedTable {
	c.Helper()
	table := observedTable(c, f.client, f.session, f.directory)
	snapshot, err := f.client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: f.session.Id, Schemas: []string{f.directory}})
	c.Assert(err, qt.IsNil)
	for i, column := range snapshot.Tables[0].Columns {
		table.Columns[i].PrimaryKey = column.Observed.PrimaryKey
	}
	return table
}

// plan plans the removal of records with the host's step that drops it, and
// renders it in form.
func (f *dropFixture) plan(c *qt.C, form protocol.RenderRequest_Form) *protocol.RenderResult {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: f.database, Schema: f.directory, Name: "records"}
	subject := testplanning.Subject(owner)
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: subject, Action: protocol.PlanningTable_DROP, Current: f.observed(c)}},
		CommonSteps: []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "drop"}, Parent: proto.CloneOf(subject),
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Fixture common operation",
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_DROP}}}}}}
	plan, err := f.client.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	rendering := testplanning.RenderRequest(plan, f.session.Capabilities)
	rendering.Form = form
	rendered, err := f.client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	return rendered
}

func (f *dropFixture) present(c *qt.C) bool {
	c.Helper()
	return slices.Contains(childNames(c, f.opened, path.Join(f.database, f.directory)), "records")
}

// TestTableDropSubprocess drops a row table and a column table through the
// native call and through the DROP TABLE a migration file holds, and reads
// the directory back.
func TestTableDropSubprocess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		definition string
		form       protocol.RenderRequest_Form
	}{
		{name: "a row table by the native call", definition: "(id Uint64, payload Utf8, PRIMARY KEY (id))", form: protocol.RenderRequest_ANY},
		{name: "a row table by text", definition: "(id Uint64, payload Utf8, PRIMARY KEY (id), INDEX by_payload GLOBAL ON (payload))",
			form: protocol.RenderRequest_TEXT},
		{name: "a column table", definition: "(ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) PARTITION BY HASH(id) WITH (STORE = COLUMN)",
			form: protocol.RenderRequest_TEXT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			f := newDropFixture(c, monitoredAddress(c), tc.definition)
			rendered := f.plan(c, tc.form)
			c.Assert(rendered.Units, qt.HasLen, 1)
			result, err := f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "drop", Units: rendered.Units})
			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
			c.Assert(f.present(c), qt.IsFalse)
		})
	}
}

// TestTableDropRefusesAChangedTableSubprocess changes the table after the plan
// read it: the native removal refuses before it drops anything, and the
// table keeps its rows.
func TestTableDropRefusesAChangedTableSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newDropFixture(c, os.Getenv("PTAH_YDB_TEST_URL"), "(id Uint64, payload Utf8, PRIMARY KEY (id))")
	rendered := f.plan(c, protocol.RenderRequest_ANY)
	absolute := path.Join(f.database, f.directory, "records")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "ALTER TABLE `"+absolute+"` ADD COLUMN added Utf8"), qt.IsNil)
	result, err := f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "stale-drop", Units: rendered.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(f.present(c), qt.IsTrue)
	// A table the plan never read is no table a removal drops either.
	_, err = f.opened.DB.ExecContext(c.Context(), "DROP TABLE `"+absolute+"`")
	c.Assert(err, qt.IsNil)
	result, err = f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "gone-drop", Units: rendered.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
}
