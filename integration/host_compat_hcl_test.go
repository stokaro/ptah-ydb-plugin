//go:build integration

package integration_test

import (
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// heldTable is what a compatibility test reads back of a table before and
// after a run: its storage, its changefeeds with their consumers, and the key
// and covered columns of each index.
type heldTable struct {
	Storage     storedTable
	Changefeeds map[string]feedShape
	Indexes     map[string][2][]string
}

// heldTableOf reads the table at absolute back from the server, without the
// provider.
func heldTableOf(c *qt.C, opened *connection.Connection, absolute string) heldTable {
	c.Helper()
	feeds, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue, qt.Commentf("no table %s", absolute))
	return heldTable{Storage: heldStorage(c, opened, absolute), Changefeeds: feeds, Indexes: heldIndexColumns(c, opened, absolute)}
}

// compatInspect inspects a fixture directory through ptah-compat as HCL and
// applies the document back with --dry-run. It returns the document, the
// notes the inspection wrote and what the apply printed, and requires that
// both runs succeed.
func compatInspect(c *qt.C, directory string) (document, notes, reapplied string) {
	c.Helper()
	document, notes, exit := runCompat(c, nil, "schema", "inspect", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory)
	c.Assert(exit, qt.Equals, 0, qt.Commentf("schema inspect:\n%s", notes))
	desired := "file://" + compatFile(c, c.TempDir(), "inspected.hcl", document)
	reapplied, applyNotes, exit := runCompat(c, nil, "schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory,
		"--to", desired, "--dry-run")
	c.Assert(exit, qt.Equals, 0, qt.Commentf("schema apply of the inspected document:\n%s\n%s\n%s", document, reapplied, applyNotes))
	return document, notes, reapplied
}

// Ported from the host's TestYDBCompatBinary_KeepsColumnFamiliesHCLCannotWrite,
// TestYDBCompatBinary_KeepsTableSettingsHCLCannotWrite and the part of
// TestYDBVectorIndexes_ApplyBackFromInspectedHCL that holds without HCL
// blocks for this provider's table settings. HCL has no spelling for a column
// family, a table's splitting settings, its changefeeds or a vector index's
// settings through this provider. A TTL has one, its platform "ydb" block
// (host_hcl_platform_test.go). `ptah-compat schema inspect` warns
// that the table's settings are not in the document, and the document applied
// back to the database it came from plans nothing: what a description leaves
// out keeps what the table holds, so the table does not return to YDB's
// defaults and loses no changefeed or index.
func TestHostCompatInspectedHCLKeepsWhatItCannotWrite_HappyPath(t *testing.T) {
	hostCompat(testcheck.New(t))
	plain := []string{"default compression=off"}
	bySize := "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0"
	for _, test := range []struct {
		name, table, capability string
		create                  []string
		want                    heldTable
	}{
		{name: "a column family", table: "docs",
			create: []string{"CREATE TABLE `{table}` (`id` Int64 NOT NULL, `n` Int32, `body` Utf8 FAMILY `cold`, PRIMARY KEY (`id`), FAMILY `cold` (COMPRESSION = 'lz4'))"},
			want: heldTable{Storage: storedTable{Families: []string{"cold compression=lz4 columns=body", "default compression=off"}, Partitioning: bySize},
				Changefeeds: map[string]feedShape{}, Indexes: map[string][2][]string{}}},
		{name: "splitting settings and a key bloom filter", table: "items",
			create: []string{"CREATE TABLE `{table}` (`id` Uint64 NOT NULL, `code` Utf8 NOT NULL, `label` Utf8, PRIMARY KEY (`id`, `code`)) WITH (" +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4, KEY_BLOOM_FILTER = ENABLED)"},
			want: heldTable{Storage: storedTable{Families: plain, Partitioning: "by_size=ENABLED size=2048 by_load=ENABLED min=4 max=0", KeyBloomFilter: "ENABLED"},
				Changefeeds: map[string]feedShape{}, Indexes: map[string][2][]string{}}},
		{name: "a changefeed and its consumer", table: "cf",
			create: []string{"CREATE TABLE `{table}` (`id` Int64 NOT NULL, `v` Int32, PRIMARY KEY (`id`))",
				"ALTER TABLE `{table}` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
				"ALTER TOPIC `{table}/feed` ADD CONSUMER `reader`"},
			want: heldTable{Storage: storedTable{Families: plain, Partitioning: bySize},
				Changefeeds: map[string]feedShape{"feed": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"reader"}}},
				Indexes:     map[string][2][]string{}}},
		{name: "a vector index", table: "vdocs", capability: "vector_indexes",
			create: []string{"CREATE TABLE `{table}` (`id` Uint64 NOT NULL, `body` Utf8, `emb` String, PRIMARY KEY (`id`), " +
				"INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`emb`) COVER (`body`) WITH (distance = cosine, vector_type = float, " +
				"vector_dimension = 3, levels = 1, clusters = 2))"},
			want: heldTable{Storage: storedTable{Families: plain, Partitioning: bySize},
				Changefeeds: map[string]feedShape{}, Indexes: map[string][2][]string{"docs_emb": {{"emb"}, {"body"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCapability(c, targetCapabilities(c), test.capability)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, test.table)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			for _, statement := range test.create {
				c.Assert(execFixtureSchema(c.Context(), opened.DB, strings.ReplaceAll(statement, "{table}", absolute)), qt.IsNil, qt.Commentf("%s", statement))
			}
			before := heldTableOf(c, opened, absolute)

			_, notes, reapplied := compatInspect(c, directory)

			c.Assert(before, qt.DeepEquals, test.want)
			c.Assert(notes, qt.Matches, `(?s).*\nwarning: table\.`+regexp.QuoteMeta(directory+"."+test.table)+`: [^\n]* is not represented in HCL\n.*`)
			c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
			c.Assert(heldTableOf(c, opened, absolute), qt.DeepEquals, test.want)
		})
	}
}

// Ported from the host's TestYDBCompatBinary_RebuildKeepsATTLTheDocumentLeavesOut,
// TestYDBCompatBinary_RebuildKeepsColumnFamiliesHCLCannotWrite and
// TestYDBCompatBinary_RebuildKeepsTableSettingsHCLCannotWrite, the documents
// TestHostRebuildFromHCLKeepsWhatTheDocumentLeavesOut_HappyPath applies with
// native ptah: ptah-compat asked for a rebuild with PTAH_ALLOW_TABLE_REBUILD=1
// writes what the table holds of a TTL, its column families and its
// splitting settings on the new table, and the next run, with or without the
// variable, finds nothing to change.
func TestHostCompatRebuildFromHCLKeepsWhatTheDocumentLeavesOut_HappyPath(t *testing.T) {
	hostCompat(testcheck.New(t))
	for _, test := range hclRebuildCases() {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			emptyAtCleanup(c, opened, path.Join(database, directory))
			c.Assert(execFixtureSchema(c.Context(), opened.DB, strings.ReplaceAll(test.create, "{table}", absolute)), qt.IsNil)
			_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, n, ts) VALUES (1l, 1, CurrentUtcTimestamp())")
			c.Assert(err, qt.IsNil)
			desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", hclItems(directory, test.nType, test.nType == "Int64"))
			apply := []string{"schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory, "--to", desired}

			rebuilt, notes, exit := runCompat(c, []string{"PTAH_ALLOW_TABLE_REBUILD=1"}, append(apply, "--auto-approve")...)
			syncedAllowed, _, syncedAllowedExit := runCompat(c, []string{"PTAH_ALLOW_TABLE_REBUILD=1"}, append(apply, "--dry-run")...)
			synced, _, syncedExit := runCompat(c, nil, append(apply, "--dry-run")...)

			c.Assert(exit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", rebuilt, notes))
			// A host lists a native operation by its kind or shows the
			// provider's statement for it.
			c.Assert(rebuilt, qt.Matches, `(?s).*(row-table-copy-operation|INSERT INTO ).*(row-table-rename-operation| RENAME TO ).*Schema apply completed successfully.*`)
			c.Assert(syncedAllowedExit, qt.Equals, 0, qt.Commentf("%s", syncedAllowed))
			c.Assert(syncedAllowed, qt.Equals, "Schema is synced, no changes to be made\n")
			c.Assert(syncedExit, qt.Equals, 0, qt.Commentf("%s", synced))
			c.Assert(synced, qt.Equals, "Schema is synced, no changes to be made\n")
			c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, test.want)
			c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(1))
		})
	}
}

// compatChangefeedDesired declares table cf of directory, whose column v is
// an Int32 or, widened, an Int64. HCL has no block for a changefeed, so the
// document says nothing about the one the table carries.
func compatChangefeedDesired(directory, vType string) string {
	return `schema "` + directory + `" {
}

table "cf" {
  schema = schema.` + directory + `
  column "id" {
    type = Int64
  }
  column "v" {
    type = ` + vType + `
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}
`
}

// Ported from the host's TestYDBCompatBinary_HCLKeepsTheChangefeedsItCannotSpell:
// an HCL document cannot spell a changefeed, so its silence about one is not
// a request to drop it. Applying the document leaves the table's changefeed
// as it is, and a rebuild the document asks for drops the changefeed before
// the swap and adds it back after, with its consumer, rather than losing it
// with the old table.
func TestHostCompatHCLKeepsTheChangefeedsItCannotSpell_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "cf")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	for _, statement := range []string{
		"CREATE TABLE `" + absolute + "` (`id` Int64 NOT NULL, `v` Int32, PRIMARY KEY (`id`))",
		"ALTER TABLE `" + absolute + "` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"ALTER TOPIC `" + absolute + "/feed` ADD CONSUMER `reader`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, v) VALUES (1l, 10), (2l, NULL)")
	c.Assert(err, qt.IsNil)
	held, found := describedChangefeeds(c, opened, absolute)
	kept := "file://" + compatFile(c, c.TempDir(), "kept.hcl", compatChangefeedDesired(directory, "Int32"))
	widened := "file://" + compatFile(c, c.TempDir(), "widened.hcl", compatChangefeedDesired(directory, "Int64"))
	apply := []string{"schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory}

	synced, syncedNotes, syncedExit := runCompat(c, nil, append(apply, "--to", kept, "--dry-run")...)
	rebuilt, rebuildNotes, rebuildExit := runCompat(c, []string{"PTAH_ALLOW_TABLE_REBUILD=1"}, append(apply, "--to", widened, "--auto-approve")...)

	c.Assert(found, qt.IsTrue)
	c.Assert(held, qt.DeepEquals, map[string]feedShape{"feed": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"reader"}}})
	c.Assert(syncedExit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", synced, syncedNotes))
	c.Assert(synced, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(rebuildExit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", rebuilt, rebuildNotes))
	c.Assert(rebuilt, qt.Contains, "Changefeed feed of table "+directory+"/cf is dropped before the swap")
	c.Assert(heldColumnTypes(c, opened, absolute)["v"], qt.Equals, "Optional<Int64>")
	after, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(after, qt.DeepEquals, held)
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}
