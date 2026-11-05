//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// feedItems declares items in directory with the changefeeds that follow.
func feedItems(directory, changefeeds string) string {
	return "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n" +
		"      n: {type: Int32}\n    changefeeds:\n" + changefeeds
}

// generatedMigration runs `ptah migrations generate` of source against the
// fixture's directory with flag, and returns the up file it writes.
func generatedMigration(c *qt.C, directory, migrations, source, flag string) string {
	c.Helper()
	output, err := hostCommand(c, "migrations", "generate", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema-file", writeYAML(c, source),
		"--schemas", directory, "--migrations-dir", migrations, "--name", "changefeeds", flag)
	c.Assert(err, qt.IsNil, qt.Commentf("migrations generate:\n%s", output))
	written, err := filepath.Glob(filepath.Join(migrations, "*.up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(written, qt.HasLen, 1, qt.Commentf("%s", output))
	text, err := os.ReadFile(written[0])
	c.Assert(err, qt.IsNil)
	return string(text)
}

// A migration file carries a changefeed change. `ptah schema diff` writes the
// statements the host's embedded engine writes for it: the drops, a changed
// changefeed among them, then the additions with their consumers, then a
// topic's change in place. `ptah migrations up` runs each statement alone,
// with no --allow-destructive, since the host's lint reads no DROP CHANGEFEED
// as a dropped column, and `ptah schema compare` finds nothing left to
// change; the changefeeds read back as declared. `ptah migrations generate` writes a down file too, and
// the host has no provider reversal of a table's property changes to write it
// from, so the file comes from the diff.
func TestHostMigrationFileCarriesChangefeedChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	f := migrationsFixture{opened: opened, dir: directory, database: database}
	created := feedItems(directory, "      gone:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          reader: {}\n"+
		"      moved:\n        mode: KEYS_ONLY\n        format: JSON\n        consumers:\n          a: {}\n          b: {}\n"+
		"      kept:\n        mode: UPDATES\n        format: JSON\n        retention_period: PT6H\n        consumers:\n          audit: {supported_codecs: raw}\n")
	changed := feedItems(directory, "      moved:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          a: {}\n          b: {}\n"+
		"      kept:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          audit: {}\n"+
		"      fresh:\n        mode: NEW_IMAGE\n        format: JSON\n")
	output, err := hostApplySource(c, directory, "yaml", created)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	migrations := c.TempDir()
	table, topic := "`"+directory+"/items`", func(name string) string { return "`" + directory + "/items/" + name + "`" }

	file, diffErr := hostCommand(c, "schema", "diff", "--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", writeYAML(c, changed), "--schemas", directory)
	c.Assert(diffErr, qt.IsNil, qt.Commentf("schema diff:\n%s", file))
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_changefeeds.up.sql"), []byte(file), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_changefeeds.down.sql"), []byte("-- The test runs no down migration.\n"), 0o600), qt.IsNil)
	up, upErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)
	compared, compareErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema-file", writeYAML(c, changed),
		"--schemas", directory, "--exit-code")

	c.Assert(file, qt.Contains, strings.Join([]string{
		"ALTER TABLE " + table + " DROP CHANGEFEED `gone`;",
		"ALTER TABLE " + table + " DROP CHANGEFEED `moved`;",
		"ALTER TABLE " + table + " ADD CHANGEFEED `fresh` WITH (MODE = 'NEW_IMAGE', FORMAT = 'JSON');",
		"ALTER TABLE " + table + " ADD CHANGEFEED `moved` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
		"ALTER TOPIC " + topic("moved") + " ADD CONSUMER `a`;",
		"ALTER TOPIC " + topic("moved") + " ADD CONSUMER `b`;",
		"ALTER TOPIC " + topic("kept") + " SET (retention_period = Interval('P1D'));",
		"ALTER TOPIC " + topic("kept") + " DROP CONSUMER `audit`;",
		"ALTER TOPIC " + topic("kept") + " ADD CONSUMER `audit`;",
	}, "\n"))
	c.Assert(upErr, qt.IsNil, qt.Commentf("migrations up:\n%s\n%s", up, file))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	shapes, found := describedChangefeeds(c, opened, path.Join(database, directory, "items"))
	c.Assert(found, qt.IsTrue)
	c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
		"moved": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"a", "b"}},
		"kept":  {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"audit"}},
		"fresh": {Mode: options.ChangefeedModeNewImage, Retention: 24 * time.Hour},
	})
}

// A migration file carries a rebuild of a table that holds changefeeds, as
// the host's embedded engine writes it: the changefeed is dropped before the
// old table moves and added with its consumer once the new table holds the
// name. `ptah migrations up` runs it with no --allow-destructive, since the
// host's lint reads the changefeed steps as part of the rebuild, the rows
// survive, and the changefeed reads back as declared.
func TestHostMigrationFileCarriesARebuildWithChangefeeds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	f := migrationsFixture{opened: opened, dir: directory, database: database}
	feeds := "      updates:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          audit: {important: true}\n"
	output, err := hostApplySource(c, directory, "yaml", feedItems(directory, feeds))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (`id`, `n`) VALUES (1ul, 10), (2ul, NULL)"), qt.IsNil)
	after := strings.Replace(feedItems(directory, feeds), "n: {type: Int32}", "n: {type: Int64}", 1)
	migrations := c.TempDir()
	table := "`" + directory + "/items`"

	file := generatedMigration(c, directory, migrations, after, "--allow-table-rebuild")
	up, upErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)
	compared, compareErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema-file", writeYAML(c, after),
		"--schemas", directory, "--exit-code")

	c.Assert(file, qt.Contains, strings.Join([]string{
		"ALTER TABLE " + table + " DROP CHANGEFEED `updates`;",
		"ALTER TABLE " + table + " RENAME TO `" + directory + "/__ptah_replaced_items`;",
		"ALTER TABLE `" + directory + "/__ptah_rebuild_items` RENAME TO " + table + ";",
		"ALTER TABLE " + table + " ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
		"ALTER TOPIC `" + directory + "/items/updates` ADD CONSUMER `audit` WITH (important = TRUE);",
	}, "\n"))
	c.Assert(upErr, qt.IsNil, qt.Commentf("migrations up:\n%s\n%s", up, file))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 1ul AND `n` = 10l"), qt.Equals, int64(1))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 2ul AND `n` IS NULL"), qt.Equals, int64(1))
	shapes, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(shapes, qt.DeepEquals, map[string]feedShape{"updates": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"audit important"}}})
}

// writeYAML writes text to a schema file of its own and returns its path.
func writeYAML(c *qt.C, text string) string {
	c.Helper()
	file := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(file, []byte(text), 0o600), qt.IsNil)
	return file
}
