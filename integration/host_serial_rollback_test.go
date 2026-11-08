//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestPlanBidirectionalSchemaDiff_SerialSequenceRollsBack
// (migration/generator): a migration that changes the increment of a Serial's
// sequence alters the sequence at its absolute path without a restart, and its
// down file puts back the increment the database held, on the same path. The
// built-in engine at 74de0b65b wrote both files so. Through this provider
// neither could be written (G232): the TEXT form named no database, since the
// host names the table it plans by none, and the down plan refused the column
// the host projects without its native observation (stokaro/ptah#5735). Both
// files apply, the sequence steps as each says, and nothing is left to plan.
func TestHostRollsBackASerialSequenceChange_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	// The revision table the migrations write goes too.
	emptyAtCleanup(c, opened, path.Join(database, directory))
	orders := path.Join(database, directory, "orders")
	sequence := path.Join(orders, "_serial_column_id")
	for _, statement := range []string{
		"CREATE TABLE `" + orders + "` (id BigSerial NOT NULL, note Utf8, PRIMARY KEY (id))",
		"ALTER SEQUENCE `" + sequence + "` INCREMENT BY 5",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	declared := "package entities\n\n//ptah:schema:table name=\"orders\" schema=\"" + directory + "\"\ntype Order struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGSERIAL\" primary=\"true\" identity_increment=\"10\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n}\n"

	migrations, up, down := hostGenerate(c, directory, "go", declared)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	stepped := describedSequence(c, opened, orders)
	settled := hostPlanned(c, directory, "go", declared)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER SEQUENCE `"+sequence+"` START WITH 1 INCREMENT BY 10;")
	c.Assert(down, qt.Contains, "ALTER SEQUENCE `"+sequence+"` START WITH 1 INCREMENT BY 5;")
	c.Assert(up+down, qt.Not(qt.Contains), "RESTART")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(stepped, qt.Equals, heldSequence{Start: 1, Increment: 10, Maximum: 9223372036854775807})
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", rolledBack))
	c.Assert(describedSequence(c, opened, orders), qt.Equals, heldSequence{Start: 1, Increment: 5, Maximum: 9223372036854775807})
}
