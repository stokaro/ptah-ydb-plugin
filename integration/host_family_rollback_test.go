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

// A down migration restores a column's old family and the default family's
// compression. YDB cannot drop the new cold family or remove a storage pool
// assigned to the default family, so the migration reports both retained
// settings. This ports TestPlanBidirectionalSchemaDiff_ColumnFamiliesRollBack
// through the host binary and a live table.
func TestHostRollsBackColumnFamilySettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "docs")
	before := replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"default\" compression=\"off\"\n"+
		"//ptah:schema:columnfamily name=\"warm\" fields=\"body\"\n", ""), directory)
	after := replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"default\" data=\""+storagePool+"\" compression=\"lz4\"\n"+
		"//ptah:schema:columnfamily name=\"warm\"\n"+
		"//ptah:schema:columnfamily name=\"cold\" data=\""+storagePool+"\" compression=\"lz4\" fields=\"body\"\n", ""), directory)
	created, createErr := hostApplySource(c, directory, "go", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{
		"default compression=off", "warm compression=off columns=body"})

	migrations, up, down := hostGenerate(c, directory, "go", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s\n%s", applied, up))
	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{
		"cold data=hdd compression=lz4 columns=body", "default data=hdd compression=lz4", "warm compression=off"})
	c.Assert(hostPlanned(c, directory, "go", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(down, qt.Contains, "Recovery limit:")
	c.Assert(down, qt.Contains, "family cold")
	c.Assert(down, qt.Contains, "DATA of family default")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/docs` ALTER FAMILY `default` SET COMPRESSION 'off', ALTER COLUMN `body` SET FAMILY `warm`;")
	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{
		"cold data=hdd compression=lz4", "default data=hdd compression=off", "warm compression=off columns=body"})
	c.Assert(hostPlanned(c, directory, "go", before), qt.HasLen, 0)
}
