//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// setStoredAttributes gives the table at absolute the user attributes given,
// as another tool would set them.
func setStoredAttributes(c *qt.C, opened *connection.Connection, absolute string, attributes map[string]string) {
	c.Helper()
	var alterations []options.AlterTableOption
	for key, value := range attributes {
		alterations = append(alterations, options.WithAddAttribute(key, value))
	}
	c.Assert(opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		return session.AlterTable(ctx, absolute, alterations...)
	}), qt.IsNil)
}

// Ported from the host's TestYDBComments_ChangesInPlace, without the index
// rows, whose comments and rename other ports hold: every kind of table and
// column comment change at once, beside a commented column dropped and one
// added. The table's comment is rewritten, a column's is removed and
// another's rewritten, and the dropped column's comment goes with it, where
// YDB would keep the attribute once the column is gone and give it to a
// column added later under that name. Attributes another tool set, one under
// no key of Ptah's and one under Ptah's prefix that names no column, are
// neither read nor removed, and nothing is left to plan. A comment ordered
// after the column's removal closed a cycle with the host's order, which puts
// a removal after the comments (`plan dependency cycle`).
//
// The table is created without its comments first, and the added column
// comes without one, since the provider path of the pinned host leaves the
// comments of a created table and of an added column out (stokaro/ptah#5256),
// and a change of comments is what the test is about. The host removes the
// dropped column's comment with a comment step (stokaro/ptah#5280).
func TestHostChangesCommentsInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "users")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	before := []string{"email", "label", "legacy"}
	applyRuns(c, directory, "yaml", commentedUsers(directory, "", before, nil))
	applyRuns(c, directory, "yaml", commentedUsers(directory, "People who sign in", before,
		map[string]string{"id": "Key", "email": "Login", "label": "Shown name", "legacy": "Going away"}))
	c.Assert(storedAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment": "People who sign in", "ptah.comment.column.id": "Key",
		"ptah.comment.column.email": "Login", "ptah.comment.column.label": "Shown name", "ptah.comment.column.legacy": "Going away"})
	setStoredAttributes(c, opened, absolute, map[string]string{"owner": "team", "ptah.comment.other": "kept"})

	applyRuns(c, directory, "yaml", commentedUsers(directory, "Customers", []string{"email", "label", "phone"},
		map[string]string{"id": "Key", "label": "Display name"}))

	c.Assert(storedAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment": "Customers", "ptah.comment.column.id": "Key",
		"ptah.comment.column.label": "Display name", "owner": "team", "ptah.comment.other": "kept"})
}

// viewBesideItems declares table items of the fixture directory, with a
// column qty where the argument asks, and a view ids over it where the
// argument asks.
func viewBesideItems(directory string, qty, view bool) string {
	text := "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n\tLabel string\n"
	if qty {
		text += "\t//ptah:schema:field name=\"qty\" type=\"Int32\"\n\tQty int32\n"
	}
	text += "}\n"
	if view {
		text += "\n//ptah:schema:view name=\"ids\" schema=\"" + directory + "\" body=\"SELECT id FROM `" + directory + "/items`\"\ntype Views struct{}\n"
	}
	return text
}

// A view the plan creates beside a column's removal is created in the same
// run, and nothing is left to plan. A view ordered after the removal closed
// a cycle with the host's order, which puts a removal after the views (`plan
// dependency cycle`).
func TestHostCreatesAViewBesideAColumnRemoval_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	applyRuns(c, directory, "go", viewBesideItems(directory, true, false))

	applyRuns(c, directory, "go", viewBesideItems(directory, false, true))

	c.Assert(storedView(c, opened, path.Join(database, directory, "ids")), qt.Equals, "SELECT id FROM `"+directory+"/items`")
	c.Assert(heldColumnNames(c, opened, path.Join(database, directory, "items")), qt.DeepEquals, []string{"id", "label"})
}

// heldColumnNames are the columns of the table at absolute, in table order.
func heldColumnNames(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	var names []string
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		names = nil
		for _, column := range described.Columns {
			names = append(names, column.Name)
		}
		return nil
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	return names
}

// A column added under the name of a commented column the plan dropped
// starts without a comment: the drop took the comment with it, where YDB
// would keep the attribute and the new column would read the old comment
// back, so the run after the addition would plan its removal.
func TestHostAddsAColumnWithoutTheCommentOfADroppedOne_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "users")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", commentedUsers(directory, "", []string{"legacy"}, nil))
	applyRuns(c, directory, "yaml", commentedUsers(directory, "", []string{"legacy"}, map[string]string{"legacy": "Going away"}))
	applyRuns(c, directory, "yaml", commentedUsers(directory, "", nil, nil))

	applyRuns(c, directory, "yaml", commentedUsers(directory, "", []string{"legacy"}, nil))

	c.Assert(storedAttributes(c, opened, absolute), qt.DeepEquals, make(map[string]string))
	c.Assert(heldColumnNames(c, opened, absolute), qt.DeepEquals, []string{"id", "legacy"})
}
