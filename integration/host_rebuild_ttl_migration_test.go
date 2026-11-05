//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// ttlItemsYAML declares table items in directory with a key id, a column n of
// nType and a timestamp seen its TTL of interval reads.
func ttlItemsYAML(directory, nType, interval string) string {
	return "tables:\n  items:\n    schema: " + directory + "\n" +
		"    platform:\n      ydb: {row_deletion_column: seen, row_deletion_interval: " + interval + "}\n" +
		"    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      n: {type: " + nType + "}\n      seen: {type: Timestamp}\n"
}

// Ported from the migration-file half of the host's
// TestYDBRebuild_CarriesTheTTL: `migrations generate --allow-table-rebuild`
// writes a rebuild whose new CREATE TABLE carries the declared TTL, so a TTL
// change in the same plan travels with it and no SET (TTL) follows, and a
// down file that rebuilds the table back with the TTL it held. `migrations up`
// leaves the table with the declared TTL, the wider column and both rows,
// with nothing left to plan; `migrations down` gives the old TTL and type
// back and keeps the rows.
func TestHostRebuildMigrationCarriesTheTTL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	hostRun(c, directory, "the table", ttlItemsYAML(directory, "Int32", "P1D"), `(?s).*Schema apply completed successfully.*`)
	f := migrationsFixture{opened: opened, dir: directory, database: database}
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (`id`, `n`, `seen`) VALUES (1l, 10, CurrentUtcTimestamp()), (2l, NULL, CurrentUtcTimestamp())"), qt.IsNil)
	after := ttlItemsYAML(directory, "Int64", "P3D")

	migrations, up, down := hostGenerate(c, directory, "yaml", after, "--allow-table-rebuild")
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward := heldStorage(c, opened, absolute).TTL
	forwardType := heldColumnTypes(c, opened, absolute)["n"]
	forwardRows := f.count(c, "SELECT COUNT(*) FROM `"+absolute+"`")
	settled := hostPlanned(c, directory, "yaml", after)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "Rows written to "+directory+"/items between the copy and the swap are lost")
	c.Assert(up, qt.Contains, "TTL = Interval(\"P3D\") ON `seen`")
	c.Assert(up, qt.Not(qt.Contains), "SET (TTL")
	c.Assert(down, qt.Contains, "TTL = Interval(\"P1D\") ON `seen`")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.Equals, "seen 259200s")
	c.Assert(forwardType, qt.Equals, "Optional<Int64>")
	c.Assert(forwardRows, qt.Equals, int64(2))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(heldStorage(c, opened, absolute).TTL, qt.Equals, "seen 86400s")
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int32>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+absolute+"`"), qt.Equals, int64(2))
}
