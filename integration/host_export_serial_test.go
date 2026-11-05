//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A Go export of a directory whose tables have Serial columns writes each as
// the host's embedded engine writes it, the column's type with auto_increment
// and the sequence's start and increment where they are not 1, and applied
// back plans nothing. Gap G46 of stokaro/ptah#4204: the provider refused the
// whole export, since it had no spelling for the sequence its read reports
// beside the column (`its sequence has no source spelling`).
func TestHostAppliesAGoExportOfSerialColumns_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	plain, stepped := path.Join(database, directory, "plain"), path.Join(database, directory, "stepped")
	createSchemaObject(c, opened, "CREATE TABLE `"+plain+"` (id BigSerial, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+plain+"`")
	createSchemaObject(c, opened, "CREATE TABLE `"+stepped+"` (id BigSerial, label Utf8, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+stepped+"`")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER SEQUENCE `"+stepped+"/_serial_column_id` START WITH 100 INCREMENT BY 5"), qt.IsNil)

	source := introspectDirectory(c, directory)

	c.Assert(source, qt.Contains, `//ptah:schema:field name="id" type="Int64" not_null="true" primary="true" auto_increment="true"`+"\n", qt.Commentf("%s", source))
	c.Assert(source, qt.Contains, `//ptah:schema:field name="id" type="Int64" not_null="true" primary="true" auto_increment="true" identity_start="100" identity_increment="5"`,
		qt.Commentf("%s", source))
	output, err := hostApplySelected(c, directory, "go", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", source, output))
}
