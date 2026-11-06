//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// keyOrderSources declare the table items of columns a, b and body, keyed by
// a and b in the order {key} names, in Go and in YAML. The key order differs
// from the column order when {key} names b first.
var keyOrderSources = map[string]string{
	"go": "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\" primary_key=\"{key}\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"a\" type=\"Uint64\" not_null=\"true\"\n\tA uint64\n" +
		"\t//ptah:schema:field name=\"b\" type=\"Utf8\" not_null=\"true\"\n\tB string\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody string\n}\n",
	"yaml": "tables:\n  items:\n    schema: {dir}\n    primary_key: [{key}]\n    columns:\n      a: {type: Uint64, not_null: true}\n" +
		"      b: {type: Utf8, not_null: true}\n      body: {type: Utf8}\n",
}

// keyOrdered is the source of format that keys items by key, in directory.
func keyOrdered(format, directory, key string) string {
	return strings.NewReplacer("{dir}", directory, "{key}", key).Replace(keyOrderSources[format])
}

// heldKey is the primary key the server holds for the table at absolute, in
// key order.
func heldKey(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	var key []string
	c.Assert(opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		key = described.PrimaryKey
		return err
	}), qt.IsNil)
	return key
}

// keyOrderFixture creates items keyed (b, a) through the host from format,
// with two rows, and returns the fixture, the directory and the table's path.
func keyOrderFixture(c *qt.C, format string) (opened *connection.Connection, directory, absolute string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyUntilSynced(c, directory, format, keyOrdered(format, directory, "b,a"))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (a, b, body) VALUES (1ul, 'x'u, 'one'u), (2ul, 'y'u, 'two'u)"), qt.IsNil)
	return opened, directory, absolute
}

// A table keyed in an order other than its columns' keeps the order: the
// server holds the declared key, a Go export writes it on the table directive
// as the host's embedded engine did (measured with the host at 74de0b65b:
// primary_key="b,a" and no field marked primary), and the export plans
// nothing applied back. Without the key in the read, the export marks each
// field primary, which reads back as the key (a, b).
func TestHostKeepsTheOrderOfACompositeKey_HappyPath(t *testing.T) {
	for _, format := range []string{"go", "yaml"} {
		t.Run(format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, absolute := keyOrderFixture(c, format)

			exported := introspectDirectory(c, directory)

			c.Assert(heldKey(c, opened, absolute), qt.DeepEquals, []string{"b", "a"})
			c.Assert(exported, qt.Contains, `primary_key="b,a"`)
			c.Assert(exported, qt.Not(qt.Contains), `primary="true"`)
			c.Assert(hostPlanned(c, directory, "go", exported), qt.HasLen, 0)
		})
	}
}

// A declaration that orders the key otherwise is refused without a rebuild,
// in the words of the host's embedded engine (measured with the host at
// 74de0b65b on 25.1.4.7 and 26.2.1.14), and the table keeps its key and its
// rows. Without the key in the read, the change compares as no change.
func TestHostRefusesAKeyOrderChangeWithoutARebuild_FailurePath(t *testing.T) {
	for _, format := range []string{"go", "yaml"} {
		t.Run(format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, absolute := keyOrderFixture(c, format)

			refused, err := hostApplySource(c, directory, format, keyOrdered(format, directory, "a,b"))

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
			c.Assert(refused, qt.Contains, "adding constraint items_pkey, the primary key, which requires target capability primary_key_alterable, "+
				"unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild")
			c.Assert(heldKey(c, opened, absolute), qt.DeepEquals, []string{"b", "a"})
			c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
		})
	}
}

// Asked for a rebuild, the change rebuilds the table with the declared key
// and its rows, and the next runs plan nothing.
func TestHostRebuildsATableForAKeyOrderChange_HappyPath(t *testing.T) {
	for _, format := range []string{"go", "yaml"} {
		t.Run(format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, absolute := keyOrderFixture(c, format)

			applyUntilSynced(c, directory, format, keyOrdered(format, directory, "a,b"), "--allow-table-rebuild")

			c.Assert(heldKey(c, opened, absolute), qt.DeepEquals, []string{"a", "b"})
			c.Assert(rowCount(c, opened, absolute, "body IS NOT NULL"), qt.Equals, int64(2))
		})
	}
}

// A migration that changes the key order rebuilds the table with the declared
// key going up and with the held key coming down, and keeps the rows both
// ways. The host's embedded engine could not write it: measured at 74de0b65b,
// `migrations generate --allow-table-rebuild` failed planning the rollback
// (`parent operation has no observed table state`).
func TestHostMigratesAKeyOrderChangeBothWays_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := keyOrderFixture(c, "go")
	migrations, up, down := hostGenerate(c, directory, "go", keyOrdered("go", directory, "a,b"), "--allow-table-rebuild")
	c.Assert(up, qt.Contains, "PRIMARY KEY (`a`, `b`)")
	c.Assert(down, qt.Contains, "PRIMARY KEY (`b`, `a`)")

	upOutput, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", upOutput))
	c.Assert(heldKey(c, opened, absolute), qt.DeepEquals, []string{"a", "b"})
	downOutput, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(downErr, qt.IsNil, qt.Commentf("%s", downOutput))
	c.Assert(heldKey(c, opened, absolute), qt.DeepEquals, []string{"b", "a"})
	c.Assert(rowCount(c, opened, absolute, "body IS NOT NULL"), qt.Equals, int64(2))
}
