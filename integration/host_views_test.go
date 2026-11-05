//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
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

// hostViews declares, in the fixture directory, a table items and a table old
// in the directory archive below it, and three views: active, whose query is
// written as an author might, with a comment, odd spacing and a trailing
// semicolon; active_count over it, declared first so that only the plan's
// order puts it after the view it reads; and old_ids in archive. The views
// directive goes in a Go string, so its backquoted paths are spelled out.
func hostViews(directory string) string {
	return strings.ReplaceAll("package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\"\ntype Item struct {\n"+
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n"+
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n\tLabel string\n\t//ptah:schema:field name=\"qty\" type=\"Int32\"\n\tQty int32\n}\n\n"+
		"//ptah:schema:table name=\"old\" schema=\"{dir}/archive\"\ntype Old struct {\n"+
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n}\n\n"+
		"//ptah:schema:view name=\"active_count\" schema=\"{dir}\" body=\"SELECT COUNT(*) AS n FROM `{dir}/active`\"\n"+
		"//ptah:schema:view name=\"active\" schema=\"{dir}\" body=\"select   id,  `label`  FROM `{dir}/items` WHERE qty>0;\"\n"+
		"//ptah:schema:view name=\"old_ids\" schema=\"{dir}/archive\" body=\"SELECT id FROM `{dir}/archive/old`\"\n"+
		"type Views struct{}\n", "{dir}", directory)
}

// storedView reads the query text the server keeps for the view at absolute.
func storedView(c *qt.C, opened *connection.Connection, absolute string) string {
	c.Helper()
	response, err := Ydb_View_V1.NewViewServiceClient(ydb.GRPCConn(opened.Driver)).DescribeView(c.Context(), &Ydb_View.DescribeViewRequest{Path: absolute})
	c.Assert(err, qt.IsNil)
	c.Assert(response.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", response.GetOperation().GetIssues()))
	var described Ydb_View.DescribeViewResult
	c.Assert(response.GetOperation().GetResult().UnmarshalTo(&described), qt.IsNil)
	return described.GetQueryText()
}

// viewsAtCleanup drops what a views test created in the fixture directory,
// the views before the tables they read, once the test ends.
func viewsAtCleanup(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
}

// Ported from the host's TestYDBViews_RoundTrip_NothingLeftToPlan and
// TestYDBViews_ReadsWhatTheServerStored: the host creates two tables and
// three views through the provider, a view over a view after the view it
// reads and a view in a directory at its path, and finds nothing to change on
// the next two runs. The server keeps each query in its own form, tokens
// joined by single spaces with no comment and no semicolon, and the views
// run: the view over a view counts the rows the view under it lets through.
func TestHostAppliesViews_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)

	applyUntilSynced(c, directory+","+directory+"/archive", "go", hostViews(directory))
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+path.Join(database, directory, "items")+"` (id, label, qty) VALUES (1l, 'a'u, 1), (2l, 'b'u, 0)")
	c.Assert(err, qt.IsNil)

	c.Assert(storedView(c, opened, path.Join(database, directory, "active")), qt.Equals, "select id , `label` FROM `"+directory+"/items` WHERE qty > 0")
	c.Assert(storedView(c, opened, path.Join(database, directory, "archive", "old_ids")), qt.Equals, "SELECT id FROM `"+directory+"/archive/old`")
	c.Assert(rowCount(c, opened, path.Join(database, directory, "active_count"), "n = 1"), qt.Equals, int64(1))
}

// Ported from the host's TestYDBViews_DroppedBeforeTheirTables: a declaration
// that leaves out every table and view drops the views first, a view before
// the view it reads, since YDB drops a table a view reads and keeps the view.
// Nothing is left to plan or to read after it. The host's test also lints the
// migration it writes for YD106; the plan here runs through the provider's
// native table drops, and the provider's YD106 parity with the host is held
// offline by TestCorpus_ChecksAsTheHost.
func TestHostDropsViewsBeforeTheirTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	selectors := directory + "," + directory + "/archive"
	created, err := hostApplySelected(c, selectors, "go", hostViews(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	const empty = "package entities\n"

	c.Assert(hostPlanned(c, selectors, "go", empty), qt.DeepEquals, []string{
		"DROP VIEW `" + directory + "/archive/old_ids`",
		"DROP VIEW `" + directory + "/active_count`",
		"DROP VIEW `" + directory + "/active`",
		"table-drop-operation " + directory + ".items",
		"table-drop-operation " + directory + "/archive.old",
	})
	output, err := hostApplySelected(c, selectors, "go", empty)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	c.Assert(hostPlanned(c, selectors, "go", empty), qt.HasLen, 0)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"archive"})
	c.Assert(listedNames(c, opened, path.Join(database, directory, "archive")), qt.HasLen, 0)
}

// Ported from the host's TestYDBViews_IntrospectedModelsPlanNothing: `ptah
// introspect` writes the views of the two directories as Go models, each with
// the query text the server stores, and the models, applied against the
// database they came from, plan nothing.
func TestHostIntrospectedViewsPlanNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	selectors := directory + "," + directory + "/archive"
	created, err := hostApplySelected(c, selectors, "go", hostViews(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))

	models := introspectDirectory(c, selectors)

	c.Assert(strings.Count(models, "//ptah:schema:view "), qt.Equals, 3, qt.Commentf("%s", models))
	output, err := hostApplySelected(c, selectors, "go", models)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", models, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", models, output))
}
