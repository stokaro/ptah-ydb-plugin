//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hostRebuildMigration is the migration `migrations generate
// --allow-table-rebuild` writes for hostRebuildFixture's change, the
// directory it is in and its text. The revision table goes in the fixture
// directory, which the cleanup empties.
func hostRebuildMigration(c *qt.C, directory, after string, f migrationsFixture) (migrations, up, down string) {
	c.Helper()
	emptyAtCleanup(c, f.opened, path.Join(f.database, f.dir))
	migrations = c.TempDir()
	generated, err := hostCommand(c, "migrations", "generate", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", after, "--schemas", directory,
		"--migrations-dir", migrations, "--name", "rebuild", "--allow-table-rebuild")
	c.Assert(err, qt.IsNil, qt.Commentf("migrations generate --allow-table-rebuild:\n%s", generated))
	up, down = generatedPair(c, migrations)
	return migrations, up, down
}

// Ported from the host's TestYDBBinary_MigrationsGenerateTakesTheFlag and the
// migrator run of TestYDBRebuild_PreservesTheRows: `migrations generate
// --allow-table-rebuild` writes the rebuild into the up file, with the note
// that rows written during it are lost, and the rebuild back into the down
// file. `migrations up` changes the column's type and keeps the rows, and
// `migrations down` takes the type back and keeps them again.
func TestHostMigrationsGenerateWritesARebuild_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixture(c)
	migrations, up, down := hostRebuildMigration(c, directory, after, f)

	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward := heldColumnTypes(c, f.opened, absolute)["n"]
	forwardRows := f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE (`id` = 1l AND `n` = 10l) OR (`id` = 2l AND `n` IS NULL)")
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "Rows written to "+directory+"/items between the copy and the swap are lost")
	c.Assert(up, qt.Contains, "CAST(`n` AS Int64)")
	c.Assert(down, qt.Contains, "-- Rebuild of table "+directory+"/items:")
	c.Assert(down, qt.Contains, "CAST(`n` AS Int32)")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.Equals, "Optional<Int64>")
	c.Assert(forwardRows, qt.Equals, int64(2))
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", rolledBack))
	c.Assert(heldColumnTypes(c, f.opened, absolute)["n"], qt.Equals, "Optional<Int32>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE (`id` = 1l AND `n` = 10) OR (`id` = 2l AND `n` IS NULL)"), qt.Equals, int64(2))
	c.Assert(objectsIn(heldEntries(c, f.opened, path.Join(f.database, directory))), qt.HasLen, 1)
}

// Ported from the host's TestYDBRebuild_ResumesAfterAFailureAfterTheCopy: a
// run of the rebuild that stops after the copy committed leaves both tables
// with the rows and a revision that records the statements before the first
// rename. The host's test stops the run with a Go observer; here a table that
// takes the name the first rename moves the old table to stops it, created
// after the generation so the plan does not choose another name. Once that
// table is gone, a rerun with --allow-dirty resumes at the first rename and
// completes the swap, and the rows are copied once.
func TestHostResumesARebuildAfterAFailureAfterTheCopy_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixture(c)
	migrations, _, _ := hostRebuildMigration(c, directory, after, f)
	obstacle := path.Join(f.database, directory, "__ptah_replaced_items")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "CREATE TABLE `"+obstacle+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)

	interrupted, failErr := hostMigrate(c, directory, migrations, "migrations", "up")
	stopped := f.progress(c, protocol.HistoryScope_NATIVE)
	copied := f.count(c, "SELECT COUNT(*) FROM `"+path.Join(f.database, directory, "__ptah_rebuild_items")+"`")
	held := f.count(c, "SELECT COUNT(*) FROM `"+absolute+"`")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "DROP TABLE `"+obstacle+"`"), qt.IsNil)
	resumed, resumeErr := hostMigrate(c, directory, migrations, "migrations", "up", "--allow-dirty")
	completed := f.progress(c, protocol.HistoryScope_NATIVE)

	c.Assert(failErr, qt.IsNotNil, qt.Commentf("up:\n%s", interrupted))
	c.Assert(stopped, qt.HasLen, 1)
	c.Assert(stopped[0].State, qt.Equals, failed)
	c.Assert(stopped[0].Total-stopped[0].Applied, qt.Equals, uint64(3))
	c.Assert(copied, qt.Equals, int64(2))
	c.Assert(held, qt.Equals, int64(2))
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("up --allow-dirty:\n%s", resumed))
	c.Assert(completed, qt.HasLen, 1)
	c.Assert(completed[0].State, qt.Equals, applied)
	c.Assert(completed[0].Applied, qt.Equals, completed[0].Total)
	c.Assert(heldColumnTypes(c, f.opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"`"), qt.Equals, int64(2))
	c.Assert(objectsIn(heldEntries(c, f.opened, path.Join(f.database, directory))), qt.HasLen, 1)
}

// Ported from the host's TestYDBRebuild_AConversionFailureLeavesTheOldTable,
// through migration files: a value the new type cannot hold fails the copy,
// which is one data query, so nothing is copied, the old table keeps its type
// and its rows, and the revision records the run as failed at the copy. Once
// the row is fixed, a rerun with --allow-dirty resumes at the copy and
// completes the rebuild.
func TestHostResumesARebuildAfterAFailedConversion_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory, absolute, after, f := hostRebuildFixtureOf(c, "TEXT", "BIGINT", "(1l, 'x'u), (2l, '5'u)")
	migrations, _, _ := hostRebuildMigration(c, directory, after, f)
	scratch := path.Join(f.database, directory, "__ptah_rebuild_items")

	failure, failErr := hostMigrate(c, directory, migrations, "migrations", "up")
	stopped := f.progress(c, protocol.HistoryScope_NATIVE)
	kept := heldColumnTypes(c, f.opened, absolute)["n"]
	unconverted := f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `n` = 'x'u")
	copied := f.count(c, "SELECT COUNT(*) FROM `"+scratch+"`")
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "UPDATE `"+absolute+"` SET `n` = '7'u WHERE `n` = 'x'u"), qt.IsNil)
	resumed, resumeErr := hostMigrate(c, directory, migrations, "migrations", "up", "--allow-dirty")

	c.Assert(failErr, qt.IsNotNil, qt.Commentf("up:\n%s", failure))
	// The built-in engine printed YDB's answer, the Unwrap message that names
	// the column. Through a provider the host prints the outcome and the
	// provider's reason, which carries the same message, and the statement
	// after them.
	c.Assert(failure, qt.Contains, "failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
		"provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement: "+
		"rebuilding table "+directory+".items: column n holds a value that does not convert to its new type (PRECONDITION_FAILED)")
	c.Assert(failure, qt.Contains, "\nSQL: INSERT INTO `"+directory+"/__ptah_rebuild_items` (`id`, `n`) SELECT ")
	c.Assert(stopped, qt.HasLen, 1)
	c.Assert(stopped[0].State, qt.Equals, failed)
	c.Assert(stopped[0].Total-stopped[0].Applied, qt.Equals, uint64(4))
	c.Assert(kept, qt.Equals, "Optional<Utf8>")
	c.Assert(unconverted, qt.Equals, int64(1))
	c.Assert(copied, qt.Equals, int64(0))
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("up --allow-dirty:\n%s", resumed))
	completed := f.progress(c, protocol.HistoryScope_NATIVE)
	c.Assert(completed, qt.HasLen, 1)
	c.Assert(completed[0].State, qt.Equals, applied)
	c.Assert(completed[0].Applied, qt.Equals, completed[0].Total)
	c.Assert(heldColumnTypes(c, f.opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"` WHERE `n` = 7l"), qt.Equals, int64(1))
	c.Assert(objectsIn(heldEntries(c, f.opened, path.Join(f.database, directory))), qt.HasLen, 1)
}
