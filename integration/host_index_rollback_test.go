//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// An index added to or dropped from a table the migration otherwise leaves
// alone rolls back as the host's embedded engine rolls it back: `migrations
// generate` writes the ALTER TABLE that drops the added index, or adds the
// dropped one again, into the down file; `migrations up` and `migrations
// down` run both files, and the first declaration plans nothing after them.
func TestHostRollsBackAnIndexChange_HappyPath(t *testing.T) {
	const index = "    indexes:\n      items_label: {fields: [label]}\n"
	for _, test := range []struct {
		name, before, after, statement string
	}{
		{name: "an added index", after: index, statement: "ALTER TABLE `{dir}/items` DROP INDEX `items_label`;"},
		{name: "a dropped index", before: index, statement: "ALTER TABLE `{dir}/items` ADD INDEX `items_label` GLOBAL SYNC ON (`label`);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			declared := func(indexes string) string {
				return "tables:\n  items:\n    schema: " + directory + "\n" + indexes +
					"    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      label: {type: Utf8}\n"
			}
			hostRun(c, directory, "the table", declared(test.before), `(?s).*Schema apply completed successfully.*`)

			migrations, _, down := hostGenerate(c, directory, "yaml", declared(test.after))
			applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
			settled := hostPlanned(c, directory, "yaml", declared(test.after))
			rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

			c.Assert(down, qt.Contains, strings.ReplaceAll(test.statement, "{dir}", directory))
			c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
			c.Assert(settled, qt.HasLen, 0)
			c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
			c.Assert(hostPlanned(c, directory, "yaml", declared(test.before)), qt.HasLen, 0)
		})
	}
}
