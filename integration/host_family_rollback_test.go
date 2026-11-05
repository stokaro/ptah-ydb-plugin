//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A column moved into another family, or back into the default one, rolls
// back as the host's embedded engine rolls it back: `migrations generate`
// writes the ALTER COLUMN that moves the column back into the down file;
// `migrations up` and `migrations down` run both files, each declaration
// plans nothing after the run that leaves it, and the table keeps its rows.
func TestHostRollsBackAColumnFamilyMove_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, before, after, statement string
	}{
		{name: "a column moved into a family", before: "body", after: "body,title",
			statement: "ALTER TABLE `{dir}/docs` ALTER COLUMN `title` SET FAMILY `default`;"},
		{name: "a column moved back to the default family", before: "body,title", after: "body",
			statement: "ALTER TABLE `{dir}/docs` ALTER COLUMN `title` SET FAMILY `cold`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "docs")
			emptyAtCleanup(c, opened, path.Join(database, directory))
			declared := func(fields string) string {
				return replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"cold\" compression=\"lz4\" fields=\""+fields+"\"\n", ""), directory)
			}
			created, err := hostApplySource(c, directory, "go", declared(test.before))
			c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
			// #nosec G202 -- the path is a table under a directory this test generated
			_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, title, body, blob) VALUES (1l, 'a'u, 'one'u, 'x'), (2l, 'b'u, NULL, NULL)")
			c.Assert(err, qt.IsNil)

			migrations, _, down := hostGenerate(c, directory, "go", declared(test.after))
			applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
			settled := hostPlanned(c, directory, "go", declared(test.after))
			rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

			c.Assert(down, qt.Contains, strings.ReplaceAll(test.statement, "{dir}", directory))
			c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
			c.Assert(settled, qt.HasLen, 0)
			c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
			c.Assert(hostPlanned(c, directory, "go", declared(test.before)), qt.HasLen, 0)
			c.Assert(rowCount(c, opened, absolute, "body = 'one'u AND blob = 'x'"), qt.Equals, int64(1))
			c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
		})
	}
}
