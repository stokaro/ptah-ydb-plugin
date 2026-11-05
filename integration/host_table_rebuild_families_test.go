//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBColumnFamilies_ARebuildKeepsWhatTheTableHolds:
// a rebuild made for another change, a column's type here, writes into the
// new table every family the old one holds and each setting the declaration
// does not state, as a cluster's table profile would have given them. The
// default family's compression and a family the declaration never names, set
// by hand on the old table, read back from the new one, the family the
// declaration names keeps the pool it held, and the rows stay.
func TestHostRebuildKeepsTheColumnFamiliesATableHolds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	inCold := "\n//ptah:schema:columnfamily name=\"cold\" fields=\"label\""
	created, err := hostApplySource(c, directory, "go", replaceDirectory(rebuiltItems(inCold, "Int32", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+absolute+"` ALTER FAMILY `default` SET COMPRESSION 'lz4', "+
		"ADD FAMILY `extra` (COMPRESSION = 'lz4'), ALTER FAMILY `cold` SET DATA '"+storagePool+"'"), qt.IsNil)
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, code, n, label) VALUES (1ul, 'a'u, 1, 'one'u), (2ul, 'b'u, 2, NULL)")
	c.Assert(err, qt.IsNil)

	rebuildRuns(c, directory, "go", replaceDirectory(rebuiltItems(inCold, "Int64", ""), directory))

	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{
		"cold data=hdd compression=off columns=label", "default compression=lz4", "extra compression=lz4",
	})
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}

// hclItems is table items of the fixture directory in HCL, which can state no
// TTL, no column family and no splitting setting: a key id, a column n of
// the type given, and a column ts that may be null as the argument says.
func hclItems(directory, nType string, tsNull bool) string {
	return strings.NewReplacer("{dir}", directory, "{n}", nType, "{null}", map[bool]string{true: "true", false: "false"}[tsNull]).Replace(
		"schema \"{dir}\" {\n}\n\ntable \"items\" {\n  schema = schema.{dir}\n  column \"id\" {\n    type = Int64\n  }\n" +
			"  column \"n\" {\n    type = {n}\n    null = true\n  }\n  column \"ts\" {\n    type = Timestamp\n    null = {null}\n  }\n" +
			"  primary_key {\n    columns = [column.id]\n  }\n}\n")
}

// hclRebuildCase is a table items created with a TTL, column families or
// splitting settings HCL cannot state, the type HCL then declares its column
// n with, and what the server holds of the table's storage before the
// rebuild and after it. Declaring the Int32 column n again makes the
// nullable column ts NOT NULL instead (see hclItems).
type hclRebuildCase struct {
	name, create, nType string
	want                storedTable
}

// hclRebuildCases are the tables the rebuilds from HCL start from, for native
// ptah and for ptah-compat alike.
func hclRebuildCases() []hclRebuildCase {
	plain := []string{"default compression=off"}
	return []hclRebuildCase{
		{name: "a TTL, for a widened column", nType: "Int64",
			create: "CREATE TABLE `{table}` (`id` Int64 NOT NULL, `n` Int32, `ts` Timestamp, PRIMARY KEY (`id`)) WITH (TTL = Interval('P1D') ON `ts`)",
			want:   storedTable{TTL: "ts 86400s", Families: plain, Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0", ReadReplicas: ""}},
		{name: "column families, for a widened column", nType: "Int64",
			create: "CREATE TABLE `{table}` (`id` Int64 NOT NULL, `n` Int32, `ts` Timestamp FAMILY `cold`, PRIMARY KEY (`id`), " +
				"FAMILY `default` (COMPRESSION = 'lz4'), FAMILY `cold` (DATA = '" + storagePool + "', COMPRESSION = 'lz4'))",
			want: storedTable{Families: []string{"cold data=hdd compression=lz4 columns=ts", "default compression=lz4"},
				Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0", ReadReplicas: ""}},
		{name: "splitting settings and read replicas, for a column made NOT NULL", nType: "Int32",
			create: "CREATE TABLE `{table}` (`id` Int64 NOT NULL, `n` Int32, `ts` Timestamp, PRIMARY KEY (`id`)) WITH (" +
				"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, READ_REPLICAS_SETTINGS = 'PER_AZ:1')",
			want: storedTable{Families: plain, Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=3 max=0", ReadReplicas: "PER_AZ:1"}},
	}
}

// Ported from the host's compatibility tests that rebuild a table from HCL:
// TestYDBCompatBinary_RebuildKeepsATTLTheDocumentLeavesOut,
// TestYDBCompatBinary_RebuildKeepsColumnFamiliesHCLCannotWrite and
// TestYDBCompatBinary_RebuildKeepsTableSettingsHCLCannotWrite. HCL can state
// no TTL, no column family and no splitting setting, so a rebuild planned
// from it, for a widened column or a column made NOT NULL, writes what the
// table holds of each on the new table rather than dropping it with the old
// one, and the next run finds nothing to change. The same document runs
// through native `schema apply --allow-table-rebuild` here and through
// ptah-compat with PTAH_ALLOW_TABLE_REBUILD=1 in
// TestHostCompatRebuildFromHCLKeepsWhatTheDocumentLeavesOut_HappyPath.
func TestHostRebuildFromHCLKeepsWhatTheDocumentLeavesOut_HappyPath(t *testing.T) {
	for _, test := range hclRebuildCases() {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			emptyAtCleanup(c, opened, path.Join(database, directory))
			c.Assert(execFixtureSchema(c.Context(), opened.DB, strings.ReplaceAll(test.create, "{table}", absolute)), qt.IsNil)
			_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, n, ts) VALUES (1l, 1, CurrentUtcTimestamp())")
			c.Assert(err, qt.IsNil)
			c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, test.want)

			rebuildRuns(c, directory, "hcl", hclItems(directory, test.nType, test.nType == "Int64"))

			c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, test.want)
			c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(1))
		})
	}
}
