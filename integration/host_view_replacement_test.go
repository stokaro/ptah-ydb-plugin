//go:build integration

package integration_test

import (
	"context"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_View_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// viewAccess declares, in directory, a table src, a view active over it with
// query, a group and a grant to the group on the view.
func viewAccess(directory string, names accessNames, query string) string {
	return ungrantedView(directory, names, query) +
		"grants:\n  view:\n    role: " + names.group + "\n    privileges: [SELECT ROW]\n    on_table: " + directory + ".active\n"
}

// ungrantedView is viewAccess without the grant: the table src, the view
// active and the group.
func ungrantedView(directory string, names accessNames, query string) string {
	return "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"views:\n  active:\n    name: " + directory + ".active\n    body: " + strconv.Quote(query) + "\n" +
		"roles:\n  " + names.group + ":\n    group: true\n"
}

// viewlessAccess is viewAccess without the view and its grant: the table src
// and the group alone.
func viewlessAccess(directory string, names accessNames) string {
	return "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"views: {}\nroles:\n  " + names.group + ":\n    group: true\n"
}

// heldView is the query the server keeps for the view at absolute, and the
// view's direct access entries.
func heldView(c *qt.C, opened *connection.Connection, absolute string) (string, []string) {
	c.Helper()
	response, err := Ydb_View_V1.NewViewServiceClient(ydb.GRPCConn(opened.Driver)).DescribeView(c.Context(), &Ydb_View.DescribeViewRequest{Path: absolute})
	c.Assert(err, qt.IsNil)
	c.Assert(response.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", response.GetOperation().GetIssues()))
	var described Ydb_View.DescribeViewResult
	c.Assert(response.GetOperation().GetResult().UnmarshalTo(&described), qt.IsNil)
	return described.GetQueryText(), accessEntries(described.GetSelf())
}

// Ported from the host's TestYDBViews_ChangedQueryIsDroppedAndCreated and
// TestYDBObjectAccess_AView. The run after the creation is synced. YDB has
// neither CREATE OR REPLACE VIEW nor ALTER VIEW, so a changed query drops the
// view and creates it again. The drop takes the view's access entries with
// it, and the plan grants the entry again after the new view. The view then
// holds the new query and the entry, and the next run is synced. A source
// that leaves the view out drops it in one statement, as the host's engine
// does: the drop takes the entry, so no REVOKE comes before it. Nothing is
// left to plan, and no entry on the view is left.
func TestHostReplacesAChangedView_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP VIEW IF EXISTS `"+path.Join(database, directory, "active")+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "src")+"`"), qt.IsNil)
	})
	source := "SELECT id FROM `" + directory + "/src`"
	for _, run := range []struct{ name, query, want string }{
		{"the creation", source, "(?s).*\nGRANT 'ydb.granular.select_row' ON `[^`]*active` TO `" + names.group + "`;\n.*Schema apply completed successfully.*"},
		{"a run after the creation", source, `(?s).*Schema is synced, no changes to be made.*`},
		{"the change", source + " WHERE id > 0",
			"(?s).*\nDROP VIEW `[^`]*active`;\nCREATE VIEW `[^`]*active` WITH \\(security_invoker = TRUE\\) AS\n[^\n]*WHERE id > 0\n;\n" +
				"GRANT 'ydb.granular.select_row' ON `[^`]*active` TO `" + names.group + "`;\n.*Schema apply completed successfully.*"},
		{"a run after it", source + " WHERE id > 0", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySelected(c, directory, "yaml", viewAccess(directory, names, run.query))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s: %s", run.name, output))
	}
	query, entries := heldView(c, opened, path.Join(database, directory, "active"))
	dropped := viewlessAccess(directory, names)
	drop := hostPlanned(c, directory, "yaml", dropped)
	output, err := hostApplySelected(c, directory, "yaml", dropped)

	c.Assert(query, qt.Equals, source+" WHERE id > 0")
	c.Assert(entries, qt.DeepEquals, []string{names.group + " ydb.granular.select_row"})
	c.Assert(err, qt.IsNil, qt.Commentf("the drop: %s", output))
	c.Assert(drop, qt.DeepEquals, []string{"DROP VIEW `" + directory + "/active`"})
	c.Assert(hostPlanned(c, directory, "yaml", dropped), qt.HasLen, 0)
	c.Assert(grantsIn(c, client, session, directory, []string{"table"}, []string{names.group}), qt.HasLen, 0)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"src"})
}

// A change of the view's query that also removes the group's grant drops the
// view and creates it again, and plans neither a REVOKE nor a GRANT: the drop
// takes the entry, and the new view has none to restore. The view holds the
// new query and no entry, and the next run is synced. The host's engine plans
// the replacement alone too.
func TestHostReplacesAViewWhoseGrantGoes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP VIEW IF EXISTS `"+path.Join(database, directory, "active")+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "src")+"`"), qt.IsNil)
	})
	source := "SELECT id FROM `" + directory + "/src`"
	changed := ungrantedView(directory, names, source+" WHERE id > 0")
	output, err := hostApplySelected(c, directory, "yaml", viewAccess(directory, names, source))
	c.Assert(err, qt.IsNil, qt.Commentf("the creation: %s", output))

	replacement := hostPlanned(c, directory, "yaml", changed)
	output, err = hostApplySelected(c, directory, "yaml", changed)
	query, entries := heldView(c, opened, path.Join(database, directory, "active"))

	c.Assert(replacement, qt.DeepEquals, []string{
		"DROP VIEW `" + directory + "/active`",
		"CREATE VIEW `" + directory + "/active` WITH (security_invoker = TRUE) AS",
	})
	c.Assert(err, qt.IsNil, qt.Commentf("the change: %s", output))
	c.Assert(query, qt.Equals, source+" WHERE id > 0")
	c.Assert(entries, qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, "yaml", changed), qt.HasLen, 0)
}

// A source that leaves out a table holding the group's grant drops the table
// and plans no REVOKE before the drop, which takes the entry, as the host's
// engine plans it. Nothing is left to plan, and the directory is empty.
func TestHostDropsATableHoldingAGrant_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "src")+"`"), qt.IsNil)
	})
	group := "roles:\n  " + names.group + ":\n    group: true\n"
	granted := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" + group +
		"grants:\n  src:\n    role: " + names.group + "\n    privileges: [SELECT ROW]\n    on_table: " + directory + ".src\n"
	dropped := "tables: {}\n" + group
	output, err := hostApplySelected(c, directory, "yaml", granted)
	c.Assert(err, qt.IsNil, qt.Commentf("the table: %s", output))
	held := grantsIn(c, client, session, directory, []string{"table"}, []string{names.group})

	drop := hostPlanned(c, directory, "yaml", dropped)
	output, err = hostApplySelected(c, directory, "yaml", dropped)

	c.Assert(held, qt.DeepEquals, []grantEntry{{Role: names.group, Privilege: "ydb.granular.select_row", Kind: "table", Name: directory + "/src"}})
	c.Assert(drop, qt.DeepEquals, []string{"table-drop-operation " + directory + ".src"})
	c.Assert(err, qt.IsNil, qt.Commentf("the drop: %s", output))
	c.Assert(hostPlanned(c, directory, "yaml", dropped), qt.HasLen, 0)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}
