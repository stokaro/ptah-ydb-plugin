//go:build integration

package integration_test

import (
	"context"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_View_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// viewAccess declares, in directory, a table src, a view active over it with
// query, a group and a grant to the group on the view.
func viewAccess(directory string, names accessNames, query string) string {
	return "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"views:\n  active:\n    name: " + directory + ".active\n    body: " + strconv.Quote(query) + "\n" +
		"roles:\n  " + names.group + ":\n    group: true\n" +
		"grants:\n  view:\n    role: " + names.group + "\n    privileges: [SELECT ROW]\n    on_table: " + directory + ".active\n"
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

// Ported from the host's TestYDBViews_ChangedQueryIsDroppedAndCreated and the
// recreation in TestYDBObjectAccess_AView. YDB has neither CREATE OR REPLACE
// VIEW nor ALTER VIEW, so a changed query drops the view and creates it
// again. The drop takes the view's access entries with it, and the plan
// grants the entry again after the new view. The view then holds the new
// query and the entry, and the next run is synced.
func TestHostReplacesAChangedView_HappyPath(t *testing.T) {
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
	for _, run := range []struct{ name, query, want string }{
		{"the creation", source, "(?s).*\nGRANT 'ydb.granular.select_row' ON `[^`]*active` TO `" + names.group + "`;\n.*Schema apply completed successfully.*"},
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
	c.Assert(query, qt.Equals, source+" WHERE id > 0")
	c.Assert(entries, qt.DeepEquals, []string{names.group + " ydb.granular.select_row"})
}
