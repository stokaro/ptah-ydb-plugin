//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
)

// TestSerialCreationNativeAndTextSubprocess creates a BigSerial that starts
// at 100 and steps by 5 twice: through the native call, and through the
// statements the TEXT form writes for a migration file. Both tables take the
// same ids and read back with the declared start and increment; the TEXT
// form's RESTART is the one difference the server records.
func TestSerialCreationNativeAndTextSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
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
		declared := &protocol.DeclaredTable{Name: name, Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "BIGSERIAL", Primary: true, IdentityStart: "100", IdentityIncrement: "5"},
				{Name: "note", Type: "Utf8", Nullable: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
		planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: negotiated(client), PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
			Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE, Desired: declared}}}
		prepareCapturedCreation(c, client, planning)
		projectCapturedCreation(c, client, planning)
		plan, err := client.PlanFeatures(ctx, planning)
		c.Assert(err, qt.IsNil)
		c.Assert(plan.Diagnostics, qt.HasLen, 0)
		rendering := testplanning.RenderRequest(client.Description(), plan, session.Capabilities)
		rendering.Form = form
		rendered, err := client.Render(ctx, rendering)
		c.Assert(err, qt.IsNil)
		c.Assert(rendered.Units, qt.HasLen, 1)
		result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "create-" + name, Units: rendered.Units})
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		return rendered
	}
	byCall := create(c, "by_call", protocol.RenderRequest_ANY)
	c.Assert(byCall.Units[0].GetNative().GetKind(), qt.Equals, tablecreate.OperationKind)
	byText := create(c, "by_text", protocol.RenderRequest_TEXT)
	c.Assert(byText.Units[0].GetSql(), qt.Equals, "CREATE TABLE `"+directory+"/by_text` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);\n"+
		"ALTER SEQUENCE `"+path.Join(database, directory, "by_text")+"/_serial_column_id` START WITH 100 INCREMENT BY 5 RESTART WITH 100;")

	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	held := make(map[string][]any)
	for _, table := range snapshot.Tables {
		id := table.Columns[0].Observed
		held[table.Identity.Name] = []any{id.ColumnType, id.IsNullable, id.AutoIncrement, id.IdentityStart, id.IdentityIncrement, id.SequenceRestart}
	}
	c.Assert(held, qt.DeepEquals, map[string][]any{
		"by_call": {"Int64", "NO", true, "100", "5", ""},
		"by_text": {"Int64", "NO", true, "100", "5", "100"},
	})
	for _, name := range []string{"by_call", "by_text"} {
		c.Assert(serialIDs(c, opened, path.Join(database, directory, name), "a", "b"), qt.DeepEquals, []int64{100, 105}, qt.Commentf("%s", name))
	}
}

// TestSerialSequenceChangeSubprocess changes the increment of an existing
// BigSerial's sequence through the native operation and through its TEXT
// form, and then refuses the same change again: the sequence no longer holds
// what the operation read.
func TestSerialSequenceChangeSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	absolute := path.Join(database, directory, "orders")
	c.Assert(execFixtureSchema(ctx, opened.DB, "CREATE TABLE `"+absolute+"` (id BigSerial NOT NULL, note Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	c.Assert(serialIDs(c, opened, absolute, "a", "b"), qt.DeepEquals, []int64{1, 2})
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "orders"}
	change := func(c *qt.C, increment int64, form protocol.RenderRequest_Form) *protocol.RenderResult {
		c.Helper()
		table, err := nativeapi.OpenTable(ctx, opened.Driver)
		c.Assert(err, qt.IsNil)
		described, err := table.Describe(ctx, absolute)
		c.Assert(err, qt.IsNil)
		c.Assert(table.Close(), qt.IsNil)
		operation, err := tablecreate.CaptureSequence("sequence", owner, &tablecreate.SequenceAlteration{Held: described.Columns[0], Settings: ydbsequence.Settings{Start: 1, Increment: increment}})
		c.Assert(err, qt.IsNil)
		rendered, err := client.Render(ctx, &protocol.RenderRequest{Engine: "ydb", Form: form, TargetFeatures: client.Description().GetEngines()[0].GetFeatures(),
			Capabilities: session.Capabilities, Changes: []*protocol.Change{{Id: "sequence", Object: proto.CloneOf(owner),
				Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation}}}}})
		c.Assert(err, qt.IsNil)
		c.Assert(rendered.Units, qt.HasLen, 1)
		return rendered
	}
	byCall := change(c, 7, protocol.RenderRequest_ANY)
	c.Assert(byCall.Units[0].GetNative().GetKind(), qt.Equals, tablecreate.SequenceOperationKind)
	result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "increment-7", Units: byCall.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	// The sequence had computed 3 as its next value before the change.
	c.Assert(serialIDs(c, opened, absolute, "c", "d"), qt.DeepEquals, []int64{1, 2, 3, 10})

	byText := change(c, 2, protocol.RenderRequest_TEXT)
	c.Assert(byText.Units[0].GetSql(), qt.Equals, "ALTER SEQUENCE `"+absolute+"/_serial_column_id` START WITH 1 INCREMENT BY 2;")
	result, err = client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "increment-2", Units: byText.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	c.Assert(serialIDs(c, opened, absolute, "e", "f"), qt.DeepEquals, []int64{1, 2, 3, 10, 17, 19})

	result, err = client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "stale", Units: byCall.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(describedSequence(c, opened, absolute), qt.Equals, heldSequence{Start: 1, Increment: 2, Maximum: 9223372036854775807})
}
