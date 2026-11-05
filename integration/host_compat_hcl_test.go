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
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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

// Ported from the host's TestYDBCompatBinary_KeepsColumnFamiliesHCLCannotWrite
// and TestYDBCompatBinary_KeepsTableSettingsHCLCannotWrite. HCL has no
// spelling for a column family, a table's splitting settings or its
// changefeeds through this provider. A TTL has one, its platform "ydb" block
// (host_hcl_platform_test.go). `ptah-compat schema inspect` reports
// that the table's settings are not in the document, and the document applied
// back to the database it came from plans nothing: what a description leaves
// out keeps what the table holds, so the table does not return to YDB's
// defaults and loses no changefeed or index.
func TestHostCompatInspectedHCLKeepsWhatItCannotWrite_HappyPath(t *testing.T) {
	hostCompat(testcheck.New(t))
	plain := []string{"default compression=off"}
	bySize := "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0"
	for _, test := range []keptCase{
		{name: "a column family", table: "docs",
			create: []string{"CREATE TABLE `{table}` (`id` Int64 NOT NULL, `n` Int32, `body` Utf8 FAMILY `cold`, PRIMARY KEY (`id`), FAMILY `cold` (COMPRESSION = 'lz4'))"},
			want: heldTable{Storage: storedTable{Families: []string{"cold compression=lz4 columns=body", "default compression=off"}, Partitioning: bySize},
				Changefeeds: make(map[string]feedShape), Indexes: make(map[string][2][]string)}},
		{name: "splitting settings and a key bloom filter", table: "items",
			create: []string{"CREATE TABLE `{table}` (`id` Uint64 NOT NULL, `code` Utf8 NOT NULL, `label` Utf8, PRIMARY KEY (`id`, `code`)) WITH (" +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4, KEY_BLOOM_FILTER = ENABLED)"},
			want: heldTable{Storage: storedTable{Families: plain, Partitioning: "by_size=ENABLED size=2048 by_load=ENABLED min=4 max=0", KeyBloomFilter: "ENABLED"},
				Changefeeds: make(map[string]feedShape), Indexes: make(map[string][2][]string)}},
		{name: "a changefeed and its consumer", table: "cf",
			create: []string{"CREATE TABLE `{table}` (`id` Int64 NOT NULL, `v` Int32, PRIMARY KEY (`id`))",
				"ALTER TABLE `{table}` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
				"ALTER TOPIC `{table}/feed` ADD CONSUMER `reader`"},
			want: heldTable{Storage: storedTable{Families: plain, Partitioning: bySize},
				Changefeeds: map[string]feedShape{"feed": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"reader"}}},
				Indexes:     make(map[string][2][]string)}},
	} {
		t.Run(test.name, func(t *testing.T) { inspectKeeps(testcheck.New(t), test) })
	}
}

// keptCase is a table whose settings HCL cannot write: the statements that
// create it and what it holds before and after its inspected document is
// applied back.
type keptCase struct {
	name, table, capability string
	create                  []string
	want                    heldTable
}

// inspectKeeps creates the table of test, inspects it with ptah-compat, which
// reports what the document cannot write, and applies the document back, which
// plans nothing and leaves the table as it was.
func inspectKeeps(c *qt.C, test keptCase) {
	c.Helper()
	requireCapability(c, targetCapabilities(c), test.capability)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, test.table)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	for _, statement := range test.create {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, strings.ReplaceAll(statement, "{table}", absolute)), qt.IsNil, qt.Commentf("%s", statement))
	}
	before := heldTableOf(c, opened, absolute)

	_, notes, reapplied := compatInspect(c, directory)

	c.Assert(before, qt.DeepEquals, test.want)
	c.Assert(notes, qt.Matches, `(?s).*table\.`+regexp.QuoteMeta(directory+"."+test.table)+`: [^\n]* (is|are) not represented in HCL.*`)
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(heldTableOf(c, opened, absolute), qt.DeepEquals, test.want)
}

// The vector index row of the host's TestYDBVectorIndexes_ApplyBackFromInspectedHCL
// for a host that does not read the provider's index kinds: HCL has no
// spelling for a vector index there, the inspection reports it, and the
// document applied back keeps it. A host that reads them writes the index
// (TestHostCompatInspectedHCLCarriesTheVectorIndex_HappyPath).
func TestHostCompatInspectedHCLKeepsAVectorIndexItCannotWrite_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostWithout(c, provider.IndexKindsFeature)
	inspectKeeps(c, keptCase{name: "a vector index", table: "vdocs", capability: "vector_indexes",
		create: []string{"CREATE TABLE `{table}` (`id` Uint64 NOT NULL, `body` Utf8, `emb` String, PRIMARY KEY (`id`), " +
			"INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`emb`) COVER (`body`) WITH (distance = cosine, vector_type = float, " +
			"vector_dimension = 3, levels = 1, clusters = 2))"},
		want: heldTable{Storage: storedTable{Families: []string{"default compression=off"}, Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0"},
			Changefeeds: make(map[string]feedShape), Indexes: map[string][2][]string{"docs_emb": {{"emb"}, {"body"}}}}})
}

// requireHostWithout skips a test whose host negotiates feature: what it
// asserts holds for a host without the feature only.
func requireHostWithout(c *qt.C, feature string) {
	c.Helper()
	host, manifest := hostManifest(c)
	if hostNegotiates(c, host, manifest, feature) {
		c.Skipf("the selected host negotiates %s", feature)
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
			_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, n, ts) VALUES (1l, 1, CurrentUtcTimestamp())") // #nosec G202 -- the path is a table under a directory this test generated
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

// compatChangefeedRebuildPlan is what ptah-compat schema apply prints for the
// rebuild of table cf of directory that widens column v, as the host's
// embedded engine at 74de0b65b printed it on 25.1.4.7 and 26.2.1.14: the
// rebuild's notes with the changefeed's among them, every setting of the new
// table, and the changefeed dropped before the swap and added after it with
// its consumer.
func compatChangefeedRebuildPlan(directory string) string {
	// A raw string cannot hold a backquote, so ~ stands for one.
	plan := `Planned schema changes:
-- Rebuild of table DIR/cf: YDB cannot make this change in place, so the table is created anew, the rows are copied, and the two tables are swapped.
-- The steps are not atomic. Rows written to DIR/cf between the copy and the swap are lost, and YDB has no lock to stop them: stop writing to the table until the last step has run.
-- The copy is one query. YDB refuses one that carries more than about 48 MiB on 25.1 or 64 MiB on 26.2; nothing is then copied, and the old table keeps serving.
-- Changefeed feed of table DIR/cf is dropped before the swap and added again after it, because YDB moves no table that carries a changefeed. Its stream restarts: the records nobody read are lost, and its consumers start again from the beginning of the new stream.
CREATE TABLE ~DIR/__ptah_rebuild_cf~ (
    ~id~ Int64 NOT NULL,
    ~v~ Int64,
    PRIMARY KEY (~id~),
    FAMILY ~default~ (COMPRESSION = 'off')
) WITH (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, KEY_BLOOM_FILTER = DISABLED);
INSERT INTO ~DIR/__ptah_rebuild_cf~ (~id~, ~v~) SELECT Unwrap(~id~, 'rebuilding table DIR.cf: column id holds NULL, and the new table declares it NOT NULL'u) AS ~id~, IF(~v~ IS NULL, NULL, Unwrap(CAST(~v~ AS Int64), 'rebuilding table DIR.cf: column v holds a value that does not convert to its new type'u)) AS ~v~ FROM ~DIR/cf~;
ALTER TABLE ~DIR/cf~ DROP CHANGEFEED ~feed~;
ALTER TABLE ~DIR/cf~ RENAME TO ~DIR/__ptah_replaced_cf~;
ALTER TABLE ~DIR/__ptah_rebuild_cf~ RENAME TO ~DIR/cf~;
ALTER TABLE ~DIR/cf~ ADD CHANGEFEED ~feed~ WITH (MODE = 'UPDATES', FORMAT = 'JSON');
ALTER TOPIC ~DIR/cf/feed~ ADD CONSUMER ~reader~;
DROP TABLE ~DIR/__ptah_replaced_cf~;
Auto-approval enabled; applying schema changes.
Schema apply completed successfully.
`
	return strings.ReplaceAll(strings.ReplaceAll(plan, "~", "`"), "DIR", directory)
}

// Ported from the host's TestYDBCompatBinary_HCLKeepsTheChangefeedsItCannotSpell:
// an HCL document cannot spell a changefeed, so its silence about one is not
// a request to drop it. Applying the document leaves the table's changefeed
// as it is, and a rebuild the document asks for drops the changefeed before
// the swap and adds it back after, with its consumer, rather than losing it
// with the old table. The plan reads as the host's embedded engine printed it.
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
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, v) VALUES (1l, 10), (2l, NULL)") // #nosec G202 -- the path is a table under a directory this test generated
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
	c.Assert(rebuilt, qt.Equals, compatChangefeedRebuildPlan(directory))
	c.Assert(heldColumnTypes(c, opened, absolute)["v"], qt.Equals, "Optional<Int64>")
	after, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(after, qt.DeepEquals, held)
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}
