//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// diffDropDirectory is the directory the migrations of
// TestHostCompatMigrateDiffDropsATable_HappyPath create their tables in.
const diffDropDirectory = "ptah_ydb_diff_drop"

// diffDropDesired keeps orders and leaves out items, which the migration
// directory created.
const diffDropDesired = `schema "` + diffDropDirectory + `" {
}

table "orders" {
  schema = schema.` + diffDropDirectory + `
  column "id" {
    type = Uint64
  }
  primary_key {
    columns = [column.id]
  }
}
`

// ptah-compat migrate diff replays a directory that created two tables on a
// dev realm and writes a migration that drops the one the desired document
// leaves out. The replay's read reserves the lock node's name at the realm's
// root, where the desired document makes no claim about streaming queries;
// the streaming planner reads the reservation as no query, so a table drop,
// which writes an input a query could read, is planned.
func TestHostCompatMigrateDiffDropsATable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	dir := c.TempDir()
	compatFile(c, dir, "20260101000000_orders.sql", "CREATE TABLE `"+diffDropDirectory+"/orders` (`id` Uint64 NOT NULL, PRIMARY KEY (`id`));\n")
	compatFile(c, dir, "20260102000000_items.sql", "CREATE TABLE `"+diffDropDirectory+"/items` (`id` Int64 NOT NULL, PRIMARY KEY (`id`));\n")
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", diffDropDesired)
	directory := "file://" + filepath.ToSlash(dir)

	hashed, hashNotes, hashExit := runCompat(c, nil, "migrate", "hash", "--dir", directory)
	diffed, diffNotes, diffExit := runCompat(c, nil, "migrate", "diff", "drop_items", "--dir", directory, "--to", desired, "--dev-url", realm.address)
	written, globErr := filepath.Glob(filepath.Join(dir, "*_drop_items.sql"))

	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s\n%s", hashed, hashNotes))
	c.Assert(diffExit, qt.Equals, 0, qt.Commentf("migrate diff:\n%s\n%s", diffed, diffNotes))
	c.Assert(globErr, qt.IsNil)
	c.Assert(written, qt.HasLen, 1)
	body, err := os.ReadFile(written[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(body), qt.Contains, "DROP TABLE `"+diffDropDirectory+"/items`")
	c.Assert(string(body), qt.Not(qt.Contains), "orders")
}
