//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A column dropped from a table rolls back as the host's embedded engine
// rolled it back: `migrations generate` writes the DROP COLUMN into the up
// file and the ADD COLUMN that restores the column into the down file, with
// the index that reads it where there is one; `migrations up` and `migrations
// down` run both files, and each declaration plans nothing after its run. The
// reverse plan restores the column from what the read holds, so the host has
// to hand this provider the column's settings as a declaration, not as the
// observation it read.
func TestHostRollsBackAColumnDrop_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, index string
		statements  []string
	}{
		{name: "a plain column", statements: []string{"ALTER TABLE `{dir}/items` ADD COLUMN `note` Utf8;"}},
		{
			name:  "a column an index reads",
			index: "    indexes:\n      items_note: {fields: [note]}\n",
			statements: []string{
				"ALTER TABLE `{dir}/items` ADD COLUMN `note` Utf8;",
				"ALTER TABLE `{dir}/items` ADD INDEX `items_note` GLOBAL SYNC ON (`note`);",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			table := "tables:\n  items:\n    schema: " + directory + "\n"
			columns := "    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      label: {type: Utf8}\n"
			before := table + test.index + columns + "      note: {type: Utf8}\n"
			after := table + columns
			hostRun(c, directory, "the table", before, `(?s).*Schema apply completed successfully.*`)

			migrations, up, down := hostGenerate(c, directory, "yaml", after)
			applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up", "--allow-destructive")
			settled := hostPlanned(c, directory, "yaml", after)
			rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

			c.Assert(up, qt.Contains, "ALTER TABLE `"+directory+"/items` DROP COLUMN `note`;")
			for _, statement := range test.statements {
				c.Assert(down, qt.Contains, strings.ReplaceAll(statement, "{dir}", directory), qt.Commentf("down:\n%s", down))
			}
			c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
			c.Assert(settled, qt.HasLen, 0)
			c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
			c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
		})
	}
}
