//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBViews_PragmaIsPartOfTheView: a view created
// the way a script that sets a path prefix creates it keeps the pragma with
// its query, because the pragma decides what the query's names mean. So the
// view is not the declaration of the same query without it: the plan drops
// the view and creates it again once, with the query the declaration gives,
// which names the whole path the pragma supplied; nothing is left to plan
// after.
func TestHostReplacesAViewWithAPragma_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	selectors := directory + "," + directory + "/archive"
	created, err := hostApplySelected(c, selectors, "go", hostViews(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	archive := path.Join(database, directory, "archive")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "DROP VIEW `"+path.Join(archive, "old_ids")+"`"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "PRAGMA TablePathPrefix('"+archive+"');\n"+
		"CREATE VIEW old_ids WITH (security_invoker = TRUE) AS SELECT id FROM old"), qt.IsNil)
	stored := storedView(c, opened, path.Join(archive, "old_ids"))

	planned := hostPlanned(c, selectors, "go", hostViews(directory))
	output, err := hostApplySelected(c, selectors, "go", hostViews(directory))

	c.Assert(stored, qt.Matches, `PRAGMA TablePathPrefix\('/\w+/`+directory+`/archive'\);\nSELECT id FROM old`)
	c.Assert(planned, qt.DeepEquals, []string{
		"DROP VIEW `" + directory + "/archive/old_ids`",
		"CREATE VIEW `" + directory + "/archive/old_ids` WITH (security_invoker = TRUE) AS",
	})
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(storedView(c, opened, path.Join(archive, "old_ids")), qt.Equals, "SELECT id FROM `"+directory+"/archive/old`")
	c.Assert(hostPlanned(c, selectors, "go", hostViews(directory)), qt.HasLen, 0)
}
