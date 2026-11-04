//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rebuiltItems declares table items keyed on an unsigned id and a text code,
// with the table attributes given, a column n of the type given, a label and
// the index directive given on the label.
func rebuiltItems(attributes, nType, index string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\" primary_key=\"id,code\"" + attributes + "\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"code\" type=\"Utf8\" not_null=\"true\"\n\tCode string\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n" + index + "\tLabel string\n}\n"
}

// rebuildRuns applies after through the host with table rebuilds allowed,
// requires that the run rebuilt the table, and that the next run, with or
// without the rebuild allowed, finds nothing to change.
func rebuildRuns(c *qt.C, directory, format, after string) {
	c.Helper()
	output, err := hostApplySelected(c, directory, format, after, "--allow-table-rebuild")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*row-table-copy-operation.*row-table-rename-operation.*Schema apply completed successfully.*`)
	c.Assert(hostPlanned(c, directory, format, after, "--allow-table-rebuild"), qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, format, after), qt.HasLen, 0)
}

// Ported from the host's TestYDBTablePartitioning_RebuildCarriesTheSettings:
// a table rebuilt for a column type change is created with the settings its
// declaration states, and nothing is left to plan.
func TestHostRebuildCarriesTheDeclaredSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	settings := ` auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="3" read_replicas_settings="ANY_AZ:1"`
	created, err := hostApplySource(c, directory, "go", replaceDirectory(rebuiltItems(settings, "Int32", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, code, n) VALUES (1ul, 'a'u, 7)")
	c.Assert(err, qt.IsNil)

	rebuildRuns(c, directory, "go", replaceDirectory(rebuiltItems(settings, "Int64", ""), directory))

	held := heldStorage(c, opened, absolute)
	c.Assert(held.Partitioning, qt.Equals, "by_size=ENABLED size=2048 by_load=ENABLED min=3 max=0")
	c.Assert(held.ReadReplicas, qt.Equals, "ANY_AZ:1")
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
}
