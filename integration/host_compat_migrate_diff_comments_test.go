//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// diffCommentsDirectory is the directory the migrations of
// TestHostCompatMigrateDiffWritesTheBuiltInChangeComments_HappyPath create
// their tables in.
const diffCommentsDirectory = "ptah_ydb_diff_comments"

// diffCommentsOrders is a table orders in diffCommentsDirectory with an
// index on its total, and the extra lines inside it that a change adds.
func diffCommentsOrders(extra string) string {
	return `table "orders" {
  schema = schema.` + diffCommentsDirectory + `
  column "id" {
    type = Uint64
  }
  column "total" {
    type = Decimal(22,9)
  }
` + extra + `  primary_key {
    columns = [column.id]
  }
  index "orders_total_ix" {
    type = "GLOBAL SYNC"
    columns = [column.total]
  }
}
`
}

// diffCommentsFirst declares orders and a table at the realm's root.
var diffCommentsFirst = `table "root_t" {
  column "id" {
    type = Int64
  }
  column "note" {
    type = Utf8
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}

schema "` + diffCommentsDirectory + `" {
}

` + diffCommentsOrders("")

// diffCommentsSecond adds a column and an index to orders and a table lines,
// and leaves out root_t.
var diffCommentsSecond = `schema "` + diffCommentsDirectory + `" {
}

` + diffCommentsOrders(`  column "note" {
    type = Utf8
    null = true
  }
`) + `
table "lines" {
  schema = schema.` + diffCommentsDirectory + `
  column "id" {
    type = Uint64
  }
  column "qty" {
    type = Int32
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}
`

// diffCommentsFiles are the migration files the host's embedded engine at
// 74de0b65b wrote for diffCommentsFirst and then diffCommentsSecond through
// ptah-compat migrate diff, on 25.1.4.7 and 26.2.1.14, by name suffix. Each
// statement carries the change the community binary names it with, an index
// added to a table its index's creation.
func diffCommentsFiles() map[string]string {
	// A raw string cannot hold a backquote, so ~ stands for one.
	files := map[string]string{
		"_first.sql": `-- Create "orders" table
CREATE TABLE ~DIR/orders~ (
    ~id~ Uint64 NOT NULL,
    ~total~ Decimal(22,9) NOT NULL,
    PRIMARY KEY (~id~),
    INDEX ~orders_total_ix~ GLOBAL SYNC ON (~total~)
);
-- Create "root_t" table
CREATE TABLE ~root_t~ (
    ~id~ Int64 NOT NULL,
    ~note~ Utf8,
    PRIMARY KEY (~id~)
);
`,
		"_second.sql": `-- Create "lines" table
CREATE TABLE ~DIR/lines~ (
    ~id~ Uint64 NOT NULL,
    ~qty~ Int32,
    PRIMARY KEY (~id~)
);
-- Modify "orders" table
ALTER TABLE ~DIR/orders~ ADD COLUMN ~note~ Utf8;
-- Create index "orders_note_ix" to table: "orders"
ALTER TABLE ~DIR/orders~ ADD INDEX ~orders_note_ix~ GLOBAL SYNC ON (~note~);
-- Drop "root_t" table
DROP TABLE ~root_t~;
`,
	}
	for name, body := range files {
		files[name] = strings.ReplaceAll(strings.ReplaceAll(body, "~", "`"), "DIR", diffCommentsDirectory)
	}
	return files
}

// writtenMigrations reads the migration files of dir whose names end with
// one of the suffixes of want, by suffix.
func writtenMigrations(c *qt.C, dir string, want map[string]string) map[string]string {
	c.Helper()
	result := make(map[string]string)
	for suffix := range want {
		names, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
		c.Assert(err, qt.IsNil)
		c.Assert(names, qt.HasLen, 1, qt.Commentf("%s", suffix))
		body, err := os.ReadFile(names[0])
		c.Assert(err, qt.IsNil)
		result[suffix] = string(body)
	}
	return result
}

// The diffCommentsSecond document with an index added to orders, as
// diffCommentsSecond declares it, adds the index on its own.
func diffCommentsSecondWithIndex() string {
	return strings.Replace(diffCommentsSecond, `  index "orders_total_ix" {`, `  index "orders_note_ix" {
    type = "GLOBAL SYNC"
    columns = [column.note]
  }
  index "orders_total_ix" {`, 1)
}

// ptah-compat migrate diff writes the files the host's embedded engine wrote
// for the same two changes, byte for byte: the statements, and above each one
// the change the community binary names, `-- Create index ... to table: ...`
// for an index added to an existing table.
func TestHostCompatMigrateDiffWritesTheBuiltInChangeComments_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	dir := c.TempDir()
	directory := "file://" + filepath.ToSlash(dir)
	first := "file://" + compatFile(c, c.TempDir(), "first.hcl", diffCommentsFirst)
	second := "file://" + compatFile(c, c.TempDir(), "second.hcl", diffCommentsSecondWithIndex())

	created, createNotes, createExit := runCompat(c, nil, "migrate", "diff", "first", "--dir", directory, "--to", first, "--dev-url", realm.address)
	changed, changeNotes, changeExit := runCompat(c, nil, "migrate", "diff", "second", "--dir", directory, "--to", second, "--dev-url", realm.address)

	c.Assert(createExit, qt.Equals, 0, qt.Commentf("migrate diff first:\n%s\n%s", created, createNotes))
	c.Assert(changeExit, qt.Equals, 0, qt.Commentf("migrate diff second:\n%s\n%s", changed, changeNotes))
	want := diffCommentsFiles()
	c.Assert(writtenMigrations(c, dir, want), qt.DeepEquals, want)
}

// diffRebuildDirectory is the directory the table that
// TestHostCompatMigrateDiffNamesARebuildAsTheBuiltIn_HappyPath rebuilds sits in.
const diffRebuildDirectory = "ptah_ydb_diff_rebuild"

// diffRebuildInit creates table t of diffRebuildDirectory with an Int32 column
// v and a changefeed, which HCL cannot declare.
const diffRebuildInit = "CREATE TABLE `" + diffRebuildDirectory + "/t` (`id` Int64 NOT NULL, `v` Int32, PRIMARY KEY (`id`));\n" +
	"ALTER TABLE `" + diffRebuildDirectory + "/t` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON');\n"

// diffRebuildWidened declares t with column v widened to Int64, a change YDB
// makes only by rebuilding the table.
const diffRebuildWidened = `schema "` + diffRebuildDirectory + `" {
}

table "t" {
  schema = schema.` + diffRebuildDirectory + `
  column "id" {
    type = Int64
  }
  column "v" {
    type = Int64
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}
`

// diffRebuildFile is the migration file the host's embedded engine at
// 74de0b65b wrote through ptah-compat migrate diff for the rebuild of t that
// diffRebuildWidened asks for, on 25.1.4.7 and 26.2.1.14: a change comment
// above the new table's creation, above each change to t and above the old
// table's drop, and none above the copy or the two renames.
func diffRebuildFile() string {
	// A raw string cannot hold a backquote, so ~ stands for one.
	file := `-- Rebuild of table DIR/t: YDB cannot make this change in place, so the table is created anew, the rows are copied, and the two tables are swapped.
-- The steps are not atomic. Rows written to DIR/t between the copy and the swap are lost, and YDB has no lock to stop them: stop writing to the table until the last step has run.
-- The copy is one query. YDB refuses one that carries more than about 48 MiB on 25.1 or 64 MiB on 26.2; nothing is then copied, and the old table keeps serving.
-- Changefeed feed of table DIR/t is dropped before the swap and added again after it, because YDB moves no table that carries a changefeed. Its stream restarts: the records nobody read are lost, and its consumers start again from the beginning of the new stream.
-- Create "__ptah_rebuild_t" table
CREATE TABLE ~DIR/__ptah_rebuild_t~ (
    ~id~ Int64 NOT NULL,
    ~v~ Int64,
    PRIMARY KEY (~id~),
    FAMILY ~default~ (COMPRESSION = 'off')
) WITH (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, KEY_BLOOM_FILTER = DISABLED);
INSERT INTO ~DIR/__ptah_rebuild_t~ (~id~, ~v~) SELECT Unwrap(~id~, 'rebuilding table DIR.t: column id holds NULL, and the new table declares it NOT NULL'u) AS ~id~, IF(~v~ IS NULL, NULL, Unwrap(CAST(~v~ AS Int64), 'rebuilding table DIR.t: column v holds a value that does not convert to its new type'u)) AS ~v~ FROM ~DIR/t~;
-- Modify "t" table
ALTER TABLE ~DIR/t~ DROP CHANGEFEED ~feed~;
ALTER TABLE ~DIR/t~ RENAME TO ~DIR/__ptah_replaced_t~;
ALTER TABLE ~DIR/__ptah_rebuild_t~ RENAME TO ~DIR/t~;
-- Modify "t" table
ALTER TABLE ~DIR/t~ ADD CHANGEFEED ~feed~ WITH (MODE = 'UPDATES', FORMAT = 'JSON');
-- Drop "__ptah_replaced_t" table
DROP TABLE ~DIR/__ptah_replaced_t~;
`
	return strings.ReplaceAll(strings.ReplaceAll(file, "~", "`"), "DIR", diffRebuildDirectory)
}

// ptah-compat migrate diff names the steps of a table rebuild as the host's
// embedded engine did, byte for byte. The rebuild's copy and its two renames
// carry no comment; one named after its subject would call the rename that
// moves the new table onto t a drop of the new table.
func TestHostCompatMigrateDiffNamesARebuildAsTheBuiltIn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	dir := c.TempDir()
	directory := "file://" + filepath.ToSlash(dir)
	c.Assert(os.WriteFile(filepath.Join(dir, "20260101000000_init.sql"), []byte(diffRebuildInit), 0o600), qt.IsNil)
	widened := "file://" + compatFile(c, c.TempDir(), "widened.hcl", diffRebuildWidened)

	hashed, hashNotes, hashExit := runCompat(c, nil, "migrate", "hash", "--dir", directory)
	diffed, diffNotes, diffExit := runCompat(c, []string{"PTAH_ALLOW_TABLE_REBUILD=1"}, "migrate", "diff", "widen", "--dir", directory, "--to", widened,
		"--dev-url", realm.address)

	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s\n%s", hashed, hashNotes))
	c.Assert(diffExit, qt.Equals, 0, qt.Commentf("migrate diff:\n%s\n%s", diffed, diffNotes))
	want := map[string]string{"_widen.sql": diffRebuildFile()}
	c.Assert(writtenMigrations(c, dir, want), qt.DeepEquals, want)
}
