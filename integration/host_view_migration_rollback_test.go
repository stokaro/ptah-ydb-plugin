//go:build integration

package integration_test

import (
	"path"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rollbackViews declares the same table on both sides of a change, with the
// views named in bodies. The before side holds gone and changed; the after
// side holds fresh and a changed query for changed.
func rollbackViews(directory, bodies string) string {
	return "tables:\n  src:\n    schema: " + directory +
		"\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\nviews:\n" + bodies
}

// A generated down migration restores the views the database held before the
// forward change. It drops the new view, recreates the removed one, and
// restores the changed view's old query by dropping and recreating it. The
// server's descriptions and the next host plan agree after each direction.
// This ports TestPlanBidirectionalSchemaDiff_YDBViewsRollBack through the host
// binary and a server instead of constructing a diff in the deleted engine.
func TestHostRollsBackAddedRemovedAndChangedViews_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	baseQuery := "SELECT id FROM `" + directory + "/src`"
	view := func(name, query string) string {
		return "  " + name + ":\n    name: " + directory + "." + name + "\n    body: " + strconv.Quote(query) + "\n"
	}
	before := rollbackViews(directory, view("gone", baseQuery)+view("changed", baseQuery+" WHERE id > 0"))
	after := rollbackViews(directory, view("fresh", baseQuery)+view("changed", baseQuery+" WHERE id > 1"))
	created, createErr := hostApplySource(c, directory, "yaml", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))

	migrations, _, down := hostGenerate(c, directory, "yaml", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(hostPlanned(c, directory, "yaml", after), qt.HasLen, 0)
	c.Assert(storedView(c, opened, path.Join(database, directory, "fresh")), qt.Equals, baseQuery)
	c.Assert(storedView(c, opened, path.Join(database, directory, "changed")), qt.Equals, baseQuery+" WHERE id > 1")

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(down, qt.Contains, "DROP VIEW `"+directory+"/changed`;")
	c.Assert(down, qt.Contains, "DROP VIEW `"+directory+"/fresh`;")
	c.Assert(down, qt.Contains, "CREATE VIEW `"+directory+"/gone`")
	c.Assert(down, qt.Contains, "CREATE VIEW `"+directory+"/changed`")
	c.Assert(strings.Index(down, "DROP VIEW `"+directory+"/changed`") < strings.Index(down, "CREATE VIEW `"+directory+"/changed`"), qt.IsTrue,
		qt.Commentf("%s", down))
	c.Assert(storedView(c, opened, path.Join(database, directory, "gone")), qt.Equals, baseQuery)
	c.Assert(storedView(c, opened, path.Join(database, directory, "changed")), qt.Equals, baseQuery+" WHERE id > 0")
	c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
	c.Assert(entryNames(objectsIn(heldEntries(c, opened, path.Join(database, directory)))), qt.DeepEquals, []string{"changed", "gone", "src"})
}
