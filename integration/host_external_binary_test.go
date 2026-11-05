//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// externalEntities is one Go source that declares a secret, a PostgreSQL data
// source that names the secret by its path, an object storage data source
// and an external table over it whose delimiter is a semicolon, in directory,
// with the secret's value read from variable.
func externalEntities(directory, variable string) string {
	return strings.NewReplacer("{dir}", directory, "{variable}", variable).Replace(`package entities

//ptah:schema:secret name="pg_password" schema="{dir}" value_env="{variable}"
//ptah:schema:externaldatasource name="warehouse" schema="{dir}" source_type="PostgreSQL" location="pg.invalid:5432" auth_method="BASIC" options="DATABASE_NAME=app;LOGIN=reader;PASSWORD_SECRET_PATH={dir}/pg_password"
//ptah:schema:externaldatasource name="bucket" schema="{dir}" source_type="ObjectStorage" location="https://storage.invalid/events/" auth_method="NONE"
type Warehouse struct{}

//ptah:schema:externaltable name="events" schema="{dir}" data_source="{dir}/bucket" location="2026/" columns="id Int64 NOT NULL, kind Utf8" options="FORMAT=csv_with_names;CSV_DELIMITER=\;"
type Event struct{}
`)
}

// Ported from the host's TestYDBBinary_AppliesExternalObjects: the host runs
// one Go source that declares external objects through this provider. The
// plan creates the secret before the data source that reads it, and the
// bucket before the external table over it; the host's engine creates the
// secret first of all, which no dependency asks for. schema compare
// --exit-code finds nothing to change, the server holds the delimiter as
// declared, and no output holds the secret's value. The test needs secrets,
// which 26.2 has and 25.1 does not.
func TestHostAppliesExternalObjectsFromOneGoSource_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL TABLE `"+path.Join(database, directory, "events")+"`")
		for _, source := range []string{"bucket", "warehouse"} {
			_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(database, directory, source)+"`")
		}
	})
	entities := writeFiles(c, c.TempDir(), map[string]string{"schema.go": externalEntities(directory, variable)})
	address := os.Getenv("PTAH_YDB_TEST_URL")

	planned := hostPlanned(c, directory, "go", externalEntities(directory, variable))
	applied, applyErr := hostRunAt(c, address, "schema", "apply", "--root-dir", entities, "--auto-approve", "--schemas", directory, "--tx-mode", "none")
	compared, compareErr := hostRunAt(c, address, "schema", "compare", "--root-dir", entities, "--exit-code", "--schemas", directory)

	secret, warehouse := "secret-operation "+directory+".pg_password", "external-data-source-operation "+directory+".warehouse"
	bucket, events := "external-data-source-operation "+directory+".bucket", "external-table-operation "+directory+".events"
	c.Assert(planned, qt.ContentEquals, []string{secret, warehouse, bucket, events})
	c.Assert(slices.Index(planned, secret) < slices.Index(planned, warehouse), qt.IsTrue, qt.Commentf("%v", planned))
	c.Assert(slices.Index(planned, bucket) < slices.Index(planned, events), qt.IsTrue, qt.Commentf("%v", planned))
	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"bucket", "events", "pg_password", "warehouse"})
	c.Assert(describeExternalTable(c, opened, path.Join(database, directory, "events")).Content["CSV_DELIMITER"], qt.Equals, `[";"]`)
	for _, text := range []string{applied, compared} {
		c.Assert(text, qt.Not(qt.Contains), "SENTINEL")
	}
}
