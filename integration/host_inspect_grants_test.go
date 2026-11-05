//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `schema inspect` reads a table that grants one user two permissions, as the
// built-in engine at host 74de0b65b read it on 25.1.4.7 and 26.2.1.14, and
// writes a GRANT for each permission. YDB describes the user once for each
// permission, and the provider refused the read (`provider convert values
// failed (InvalidArgument)`, gap G147), because a declaration names each
// subject once. A grant the provider applied itself, one GRANT per
// permission, is read back the same way, so every inspection of a database it
// granted two permissions on one object failed.
func TestHostInspectsAUserGrantedTwoPermissions_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	user := "ptahg147" + strings.ToLower(rand.Text())[:8]
	table := path.Join(database, directory, "t")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	for _, statement := range []string{
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"CREATE USER `" + user + "`",
		"GRANT SELECT ROW, UPDATE ROW ON `" + table + "` TO `" + user + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "GRANT 'ydb.granular.select_row' ON `"+directory+"/t` TO `"+user+"`;")
	c.Assert(output, qt.Contains, "GRANT 'ydb.granular.update_row' ON `"+directory+"/t` TO `"+user+"`;")
}
