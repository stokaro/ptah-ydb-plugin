//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A migration that creates a table whose Serial key starts at 100 and steps
// by 5 writes, after the CREATE TABLE, the ALTER SEQUENCE the built-in engine
// at 74de0b65b wrote on 25.1.4.7 and 26.2.1.14, on the sequence's absolute
// path, and the file creates the sequence with both settings: YDB records the
// restart, the first rows take 100 and 105, and nothing is left to plan.
// Through this provider the up file held the CREATE TABLE alone (G238): the
// host names the table it plans by no database, and the text form took the
// database from the operation's owner, so it wrote no ALTER SEQUENCE, and the
// sequence started at 1 and stepped by 1.
func TestHostMigratesASerialCreatedWithItsSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	// The revision table the migrations write goes too.
	emptyAtCleanup(c, opened, path.Join(database, directory))
	orders := path.Join(database, directory, "orders")
	declared := "package entities\n\n//ptah:schema:table name=\"orders\" schema=\"" + directory + "\"\ntype Order struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGSERIAL\" primary=\"true\" identity_start=\"100\" identity_increment=\"5\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n}\n"

	migrations, up, _ := hostGenerate(c, directory, "go", declared)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	settled := hostPlanned(c, directory, "go", declared)

	c.Assert(up, qt.Contains, "CREATE TABLE `"+path.Join(directory, "orders")+"` (")
	c.Assert(up, qt.Contains, "ALTER SEQUENCE `"+orders+"/_serial_column_id` START WITH 100 INCREMENT BY 5 RESTART WITH 100;")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(describedSequence(c, opened, orders), qt.Equals, heldSequence{Start: 100, Increment: 5, Restart: 100, Maximum: 9223372036854775807})
	c.Assert(serialIDs(c, opened, orders, "a", "b"), qt.DeepEquals, []int64{100, 105})
	c.Assert(settled, qt.HasLen, 0)
}
