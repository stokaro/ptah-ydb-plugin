//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// twoViews declares table users and the views fresh and kept over it, each
// with a comment, fresh with the body given.
func twoViews(directory, freshBody, freshComment, keptComment string) string {
	return "package entities\n\n//ptah:schema:table name=\"users\" schema=\"" + directory + "\"\ntype User struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n}\n\n" +
		"//ptah:schema:view name=\"fresh\" schema=\"" + directory + "\" body=\"" + freshBody + "\" comment=\"" + freshComment + "\"\ntype Fresh struct{}\n\n" +
		"//ptah:schema:view name=\"kept\" schema=\"" + directory + "\" body=\"SELECT id FROM `" + directory + "/users`\" comment=\"" + keptComment + "\"\ntype Kept struct{}\n"
}

// Ported from the host's TestGenerateMigrationAST_ViewComments_HappyPath: a
// view the plan drops and creates again for a new query writes its comment
// with its CREATE VIEW, and a view the plan keeps takes its changed comment
// in place, after every view is created. Both comments are held afterwards,
// and nothing is left to plan.
func TestHostRecreatesAViewWithItsComment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applied, err := hostApplySource(c, directory, "go", twoViews(directory, "SELECT 1 AS a", "Old", "Old"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	changed := twoViews(directory, "SELECT 2 AS a", "Recreated", "New")

	planned := hostPlanned(c, directory, "go", changed)
	applied, err = hostApplySource(c, directory, "go", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))

	c.Assert(planned, qt.DeepEquals, []string{
		"DROP VIEW `" + directory + "/fresh`",
		"CREATE VIEW `" + directory + "/fresh` WITH (security_invoker = TRUE) AS",
		"COMMENT ON VIEW `" + directory + "/kept` IS 'New'",
	})
	c.Assert(heldAttributes(c, opened, path.Join(database, directory, "fresh")), qt.DeepEquals, map[string]string{"ptah.comment": "Recreated"})
	c.Assert(heldAttributes(c, opened, path.Join(database, directory, "kept")), qt.DeepEquals, map[string]string{"ptah.comment": "New"})
	c.Assert(hostPlanned(c, directory, "go", changed), qt.HasLen, 0)
}

// Ported from the dropped-table part of the host's
// TestGenerateMigrationAST_IndexComments_HappyPath: the comment of an index
// on a table the plan drops goes with the table, and the plan has no
// statement for it.
func TestHostDropsATableWithItsIndexComment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	legacy := "package entities\n\n//ptah:schema:table name=\"legacy\" schema=\"" + directory + "\"\ntype Legacy struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"v\" type=\"Utf8\"\n\t//ptah:schema:index name=\"legacy_ix\" fields=\"v\" comment=\"Gone with the table\"\n\tV string\n}\n"
	applied, err := hostApplySource(c, directory, "go", legacy)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))

	planned := hostPlanned(c, directory, "go", "package entities\n")

	c.Assert(planned, qt.DeepEquals, []string{"table-drop-operation " + directory + ".legacy"})
}

// Ported from the "a comment declared in the rename" half of the host's
// TestGenerateMigrationAST_IndexComments_RenameHalves: an index renamed from a
// name that held no comment to one the declaration comments takes the
// comment under its new name alone, with nothing to clear under the old one.
// TestHostMovesIndexCommentsWithRename_HappyPath holds the half where the
// rename removes a comment. Both compare against the index YDB holds.
func TestHostRenamesAnIndexIntoAComment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	from := indexCommentDocument(", INDEX old GLOBAL ON (a)", "")
	to := indexCommentDocument(", INDEX new GLOBAL ON (a)", indexCommentClause("new", "New"))

	out := indexCommentDiff(c, from, to)

	c.Assert(out, qt.Contains, "RENAME INDEX `old` TO `new`")
	c.Assert(indexCommentStatements(out), qt.DeepEquals, []string{"COMMENT ON INDEX `new` ON `docs` IS 'New';"}, qt.Commentf("%s", out))
}
