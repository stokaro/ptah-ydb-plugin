//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A change of a global index's partitioning and read replicas rolls back as
// the host's embedded engine rolls it back: `migrations generate` writes the
// ALTER INDEX ... SET that gives each changed index every setting it held,
// read replicas removed by the forward change included, and says that a
// maximum partition count the forward change gave an index stays, since YDB
// cannot remove one. `migrations up` and `migrations down` run both files,
// each index holds its settings again, and the first declaration plans
// nothing after them.
func TestHostRollsBackIndexSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ObservedIndexFacetsFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := indexSettingsItems(directory, ` read_replicas_settings="PER_AZ:1"`, "by_n", ` auto_partitioning_min_partitions_count="2"`, indexHolder("by_id_body", ""))
	after := indexSettingsItems(directory, ` read_replicas_settings="PER_AZ:0"`, "by_n", ` auto_partitioning_min_partitions_count="3" auto_partitioning_by_load="ENABLED"`,
		indexHolder("by_id_body", ` auto_partitioning_max_partitions_count="9"`))
	created, createErr := hostApply(c, directory, before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	held := heldSettings(c, opened, absolute)

	migrations, _, down := hostGenerate(c, directory, "go", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward := heldSettings(c, opened, absolute)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` ALTER INDEX `by_n` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` ALTER INDEX `by_body` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:1\");")
	c.Assert(down, qt.Contains, `"YDB cannot remove a maximum partition count, so the rollback keeps AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9, which the forward change set."`)
	c.Assert(down, qt.Not(qt.Contains), "ALTER INDEX `by_id_body`")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.DeepEquals, map[string]string{"by_body": "min=1 load=DISABLED", "by_n": "min=3 load=ENABLED", "by_id_body": "min=1 load=DISABLED"})
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, held)
	c.Assert(held, qt.DeepEquals, map[string]string{"by_body": "min=1 load=DISABLED replicas=PER_AZ:1", "by_n": "min=2 load=DISABLED", "by_id_body": "min=1 load=DISABLED"})
	c.Assert(hostPlanned(c, directory, "go", before), qt.HasLen, 0)
}

// The reverse of a rename and two in-place index setting changes names the
// renamed index under its old name before restoring its settings. It also
// restores the other index's default splitting policy, which a declaration
// that omits the setting would otherwise leave enabled. This ports
// TestPlanBidirectionalSchemaDiff_IndexChangesInPlaceRollBack through the host
// binary and a live table.
func TestHostRollsBackRenamedIndexesWithSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ObservedIndexFacetsFeature)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := indexSettingsItems(directory, "", "by_n", ` auto_partitioning_min_partitions_count="3"`, "")
	after := indexSettingsItems(directory, ` auto_partitioning_by_load="ENABLED"`, "by_number", ` auto_partitioning_min_partitions_count="4"`, "")
	created, createErr := hostApply(c, directory, before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=1 load=DISABLED", "by_n": "min=3 load=DISABLED"})

	migrations, up, down := hostGenerate(c, directory, "go", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s\n%s", applied, up))
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=1 load=ENABLED", "by_number": "min=4 load=DISABLED"})
	c.Assert(hostPlanned(c, directory, "go", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` RENAME INDEX `by_number` TO `by_n`;")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` ALTER INDEX `by_n` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` ALTER INDEX `by_body` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1);")
	c.Assert(strings.Index(down, "RENAME INDEX `by_number` TO `by_n`") < strings.Index(down, "ALTER INDEX `by_n` SET"), qt.IsTrue, qt.Commentf("%s", down))
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=1 load=DISABLED", "by_n": "min=3 load=DISABLED"})
	c.Assert(hostPlanned(c, directory, "go", before), qt.HasLen, 0)
}

// A rebuild gives each index of the new table the partitioning and read
// replicas its own facet declares and, for every setting the facet leaves out,
// what the old index holds, as the host's embedded engine writes ALTER INDEX
// ... SET for every index of a rebuilt table; an index the rebuild adds takes
// the settings it declares. The rollback rebuilds the table back and gives
// each index every setting it held. The embedded engine's rollback kept a
// setting the forward change gave an index where the first declaration leaves
// it out (AUTO_PARTITIONING_BY_LOAD = ENABLED here); this provider restores it,
// deliberately.
func TestHostRebuildGivesTheIndexesTheirDeclaredSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ObservedIndexFacetsFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := typedIndexSettingsItems(directory, "Int32", ` read_replicas_settings="PER_AZ:1"`, "by_n", ` auto_partitioning_min_partitions_count="2"`, "")
	after := typedIndexSettingsItems(directory, "Int64", "", "by_n", ` auto_partitioning_min_partitions_count="3" auto_partitioning_by_load="ENABLED"`,
		indexHolder("by_id_body", ` auto_partitioning_min_partitions_count="4"`))
	created, createErr := hostApply(c, directory, before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	held := heldSettings(c, opened, absolute)

	migrations, up, down := hostGenerate(c, directory, "go", after, "--allow-table-rebuild")
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward := heldSettings(c, opened, absolute)
	settled := hostPlanned(c, directory, "go", after, "--allow-table-rebuild")
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER TABLE `"+directory+"/__ptah_rebuild_items` ALTER INDEX `by_n` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/__ptah_rebuild_items` ALTER INDEX `by_n` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, "+
		"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.DeepEquals, map[string]string{"by_body": "min=1 load=DISABLED replicas=PER_AZ:1", "by_n": "min=3 load=ENABLED", "by_id_body": "min=4 load=DISABLED"})
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, held)
	c.Assert(held, qt.DeepEquals, map[string]string{"by_body": "min=1 load=DISABLED replicas=PER_AZ:1", "by_n": "min=2 load=DISABLED"})
	c.Assert(hostPlanned(c, directory, "go", before, "--allow-table-rebuild"), qt.HasLen, 0)
}

// rebuildIndexItems declares items in directory with n of type nType and the
// index directives given on n, on body and on a holder field.
func rebuildIndexItems(directory, nType, nIndex, bodyIndex, holder string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n" + nIndex + "\tN int64\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n" + bodyIndex + "\tBody string\n" + holder + "}\n"
}

// A rebuild takes the host's steps for its table, and the host's plan graph
// requires the steps they are rewritten to to name every object they change.
// So a rebuild that also adds, drops or redefines an index, or adds a column,
// applies as the host's embedded engine applies it: the new table holds the
// declared indexes and columns, and the next run plans nothing.
func TestHostRebuildTakesTheIndexAndColumnSteps_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := rebuildIndexItems(directory, "Int32", "\t//ptah:schema:index name=\"by_n\" fields=\"n\"\n", "\t//ptah:schema:index name=\"by_body\" fields=\"body\"\n", "")
	after := rebuildIndexItems(directory, "Int64", "\t//ptah:schema:index name=\"by_n\" fields=\"n,body\"\n", "",
		"\t//ptah:schema:field name=\"extra\" type=\"Utf8\"\n\t//ptah:schema:index name=\"by_extra\" fields=\"extra\"\n\tExtra string\n")
	created, createErr := hostApply(c, directory, before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))

	rebuilt, rebuildErr := hostApplySelected(c, directory, "go", after, "--allow-table-rebuild")

	c.Assert(rebuildErr, qt.IsNil, qt.Commentf("%s", rebuilt))
	c.Assert(rebuilt, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{"by_n": {{"n", "body"}, nil}, "by_extra": {{"extra"}, nil}})
	c.Assert(heldColumnTypes(c, opened, absolute), qt.DeepEquals, map[string]string{"id": "Uint64", "n": "Optional<Int64>", "body": "Optional<Utf8>", "extra": "Optional<Utf8>"})
	c.Assert(hostPlanned(c, directory, "go", after, "--allow-table-rebuild"), qt.HasLen, 0)
}
