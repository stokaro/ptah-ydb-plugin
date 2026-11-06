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
