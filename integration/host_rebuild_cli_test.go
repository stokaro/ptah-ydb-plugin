//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rebuildEntities is a table whose `n` column is declared with the type passed
// in, so two versions of it change the column's type, which YDB cannot change
// in place. An empty schema puts it at the database root.
func rebuildEntities(schema, nType string) string {
	directive := "//ptah:schema:table name=\"items\""
	if schema != "" {
		directive += " schema=\"" + schema + "\""
	}
	return "package entities\n\n" + directive + "\ntype Item struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n}\n"
}

// hostRebuildFixture is a table of the fixture's directory created with `n` as
// INTEGER and two rows, and the directory that declares it with `n` as BIGINT.
func hostRebuildFixture(c *qt.C) (directory, absolute, after string, f migrationsFixture) {
	c.Helper()
	return hostRebuildFixtureOf(c, "INTEGER", "BIGINT", "(1l, 10), (2l, NULL)")
}

// hostRebuildFixtureOf is a table of the fixture's directory created with `n`
// of type from and the rows given as the VALUES of (`id`, `n`), and the
// directory that declares it with `n` of type to.
func hostRebuildFixtureOf(c *qt.C, from, to, rows string) (directory, absolute, after string, f migrationsFixture) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{absolute, path.Join(database, directory, "__ptah_rebuild_items"), path.Join(database, directory, "__ptah_replaced_items")} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+table+"`"), qt.IsNil)
		}
	})
	before := writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildEntities(directory, from)})
	after = writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildEntities(directory, to)})
	created, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", before, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("schema apply:\n%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (`id`, `n`) VALUES "+rows), qt.IsNil)
	return directory, absolute, after, migrationsFixture{opened: opened, dir: directory, database: database}
}

// `schema apply` of a column type change YDB cannot make in place rebuilds
// the table when asked: the plan the host prints warns that rows written
// during the rebuild are lost, the table is rebuilt with its rows, and `schema
// compare` finds nothing left to change.
func TestHostSchemaApplyRebuildsWhenAsked_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixture(c)

	rebuilt, rebuildErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none", "--allow-table-rebuild")
	compared, compareErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after,
		"--schemas", directory, "--exit-code")

	c.Assert(rebuildErr, qt.IsNil, qt.Commentf("schema apply --allow-table-rebuild:\n%s", rebuilt))
	c.Assert(rebuilt, qt.Contains, "Rows written to "+directory+"/items between the copy and the swap are lost")
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 1l AND `n` = 10l"), qt.Equals, int64(1))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 2l AND `n` IS NULL"), qt.Equals, int64(1))
	c.Assert(f.names(c), qt.DeepEquals, []string{"items"})
}

// Ported from the `schema apply` half of the host's
// TestYDBRebuild_AConversionFailureLeavesTheOldTable: a value the new type
// cannot hold fails the copy, which is one data query, so nothing is copied
// and the old table keeps its type and its rows. The host's embedded engine
// printed YDB's answer, the copy's check that names the table and the column;
// through a provider the run stops after the creation of the new table, with
// the same sentence in the reason.
func TestHostSchemaApplyNamesTheColumnARebuildCannotConvert_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixtureOf(c, "TEXT", "BIGINT", "(1l, 'x'u), (2l, '5'u)")

	failure, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none", "--allow-table-rebuild")

	c.Assert(err, qt.IsNotNil, qt.Commentf("schema apply --allow-table-rebuild:\n%s", failure))
	c.Assert(failure, qt.Contains, "provider execution: PARTIAL (1 units confirmed applied): YDB refused the statement: "+
		"rebuilding table "+directory+".items: column n holds a value that does not convert to its new type (PRECONDITION_FAILED)")
	c.Assert(heldColumnTypes(c, f.opened, absolute)["n"], qt.Equals, "Optional<Utf8>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `n` = 'x'u"), qt.Equals, int64(1))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+path.Join(f.database, directory, "__ptah_rebuild_items")+"`"), qt.Equals, int64(0))
}

// Without --allow-table-rebuild the apply is refused, names the flag, and
// changes nothing.
func TestHostSchemaApplyRefusesARebuildNotAskedFor_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixture(c)

	refused, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none")

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Contains, "plans when asked with --allow-table-rebuild")
	c.Assert(heldColumnTypes(c, f.opened, absolute)["n"], qt.Equals, "Optional<Int32>")
}

// planningCommand is a native command that plans schema changes, and the text
// of the rebuild's swap it prints: the YQL statement where the command prints
// statements, and the provider's native operation where it prints a plan of
// steps, as `schema plan` does through a provider where the built-in engine
// prints the statement.
type planningCommand struct {
	args []string
	swap string
}

// planningCommands are the native commands that plan schema changes, given a
// dev realm's address and the table declared with `n` as BIGINT, in Go
// annotations and in a YAML schema file, which names no directory.
func planningCommands(c *qt.C, address, after, afterYAML string) map[string]planningCommand {
	c.Helper()
	const statement = "ALTER TABLE `__ptah_rebuild_items` RENAME TO `items`"
	return map[string]planningCommand{
		"schema compare": {[]string{"schema", "compare", "--db-url", address, "--root-dir", after}, statement},
		"schema diff":    {[]string{"schema", "diff", "--from", address, "--to", afterYAML, "--include", "items"}, statement},
		"schema plan": {[]string{"schema", "plan", "--db-url", address, "--root-dir", after, "--name", "rebuild", "--output", filepath.Join(c.TempDir(), "rebuild.plan.hcl")},
			"Native operation: github.com/stokaro/ptah-ydb-plugin/row-table-rename-operation\n  __ptah_rebuild_items: Renames a table"},
		"migrations plan": {[]string{"migrations", "plan", "--db-url", address, "--root-dir", after}, statement},
	}
}

// rebuildRealm is a dev realm whose root holds the table with `n` as INTEGER,
// and the sources that declare it with `n` as BIGINT.
func rebuildRealm(c *qt.C) (realm cliRealm, after, afterYAML string) {
	c.Helper()
	realm = newCLIRealm(c)
	before := writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildEntities("", "INTEGER")})
	after = writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildEntities("", "BIGINT")})
	afterYAML = filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"after.yaml": "tables:\n  items:\n    columns:\n      id:\n" +
		"        type: BIGINT\n        primary: true\n      n:\n        type: BIGINT\n"}), "after.yaml")
	created, err := hostCommand(c, "schema", "apply", "--db-url", realm.address, "--root-dir", before, "--auto-approve", "--tx-mode", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("schema apply:\n%s", created))
	return realm, after, afterYAML
}

// Each native command that plans schema changes plans the rebuild when asked
// with --allow-table-rebuild, notes included.
func TestHostEveryPlanningCommandTakesTheRebuildFlag_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm, after, afterYAML := rebuildRealm(c)
	for name, command := range planningCommands(c, realm.address, after, afterYAML) {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)

			planned, err := hostCommand(c, append(command.args, "--allow-table-rebuild")...)

			c.Assert(err, qt.IsNil, qt.Commentf("%s --allow-table-rebuild:\n%s", name, planned))
			c.Assert(planned, qt.Contains, "-- Rebuild of table items:")
			c.Assert(planned, qt.Contains, command.swap)
		})
	}
}

// Without the flag each planning command is refused with a message naming it,
// and so is `migrations generate`, which writes no file.
func TestHostEveryPlanningCommandRefusesARebuildNotAskedFor_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	realm, after, afterYAML := rebuildRealm(c)
	commands := planningCommands(c, realm.address, after, afterYAML)
	generated := c.TempDir()
	commands["migrations generate"] = planningCommand{args: []string{"migrations", "generate", "--db-url", realm.address, "--root-dir", after, "--migrations-dir", generated, "--name", "rebuild"}}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)

			refused, err := hostCommand(c, command.args...)

			c.Assert(err, qt.IsNotNil)
			c.Assert(refused, qt.Contains, "plans when asked with --allow-table-rebuild")
		})
	}
	written, err := filepath.Glob(filepath.Join(generated, "*.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(written, qt.HasLen, 0)
}

// rebuildFeedEntities is rebuildEntities in directory with the changefeed
// updates on the table and its consumer audit.
func rebuildFeedEntities(directory, nType string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\n" +
		"//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\" retention_period=\"PT6H\"\n" +
		"//ptah:schema:changefeed:consumer name=\"audit\" changefeed=\"updates\" important=\"true\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n}\n"
}

// Ported from the host's TestYDBChangefeeds_RebuildCarriesThem. YDB moves no
// table that carries a changefeed, so the rebuild drops the changefeed before
// it moves the old table and adds it with its consumer to the new one, and the
// plan says the stream restarts. The rows survive, the changefeed reads back
// as declared, and `schema compare` finds nothing left to change.
func TestHostSchemaApplyRebuildCarriesChangefeeds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	f := migrationsFixture{opened: opened, dir: directory, database: database}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{absolute, path.Join(database, directory, "__ptah_rebuild_items"), path.Join(database, directory, "__ptah_replaced_items")} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+table+"`"), qt.IsNil)
		}
	})
	before := writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildFeedEntities(directory, "INTEGER")})
	after := writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildFeedEntities(directory, "BIGINT")})
	created, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", before, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("schema apply:\n%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (`id`, `n`) VALUES (1l, 10), (2l, NULL)"), qt.IsNil)

	rebuilt, rebuildErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none", "--allow-table-rebuild")
	compared, compareErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after,
		"--schemas", directory, "--exit-code")

	c.Assert(rebuildErr, qt.IsNil, qt.Commentf("schema apply --allow-table-rebuild:\n%s", rebuilt))
	c.Assert(rebuilt, qt.Contains, "Changefeed updates of table "+directory+"/items is dropped before the swap and added again after it, "+
		"because YDB moves no table that carries a changefeed.")
	c.Assert(rebuilt, qt.Contains, "Its stream restarts")
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 1l AND `n` = 10l"), qt.Equals, int64(1))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `id` = 2l AND `n` IS NULL"), qt.Equals, int64(1))
	c.Assert(f.names(c), qt.DeepEquals, []string{"items"})
	shapes, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(shapes, qt.DeepEquals, map[string]feedShape{"updates": {Mode: options.ChangefeedModeUpdates, Retention: 6 * time.Hour, Consumers: []string{"audit important"}}})
}
