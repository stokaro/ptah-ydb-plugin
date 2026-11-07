//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from TestGenerateMigrationAST_External_Order. A plan drops an
// external table before the old source it reads, creates a new table after
// its source, creates a password secret before the source that reads it, and
// places a view over the new table after the external objects. Independent
// external operations retain the old YDB planner's name order.
func TestHostOrdersExternalObjectsAgainstSecretsAndViews_HappyPath(t *testing.T) {
	variable := secretVariable(t, "probe")
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	oldSource := path.Join(database, directory, "old")
	staleTable := path.Join(database, directory, "stale")
	createSchemaObject(c, opened,
		"CREATE EXTERNAL DATA SOURCE `"+oldSource+"` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.test/old/', AUTH_METHOD = 'NONE')",
		"DROP EXTERNAL DATA SOURCE `"+oldSource+"`")
	createSchemaObject(c, opened,
		"CREATE EXTERNAL TABLE `"+staleTable+"` (id Int64 NOT NULL) WITH (DATA_SOURCE = '"+oldSource+"', LOCATION = 'x/', FORMAT = 'json_each_row')",
		"DROP EXTERNAL TABLE `"+staleTable+"`")

	source := strings.NewReplacer("{dir}", directory, "{variable}", variable,
		"{query}", "SELECT id FROM `"+directory+"/events`").Replace(`secrets:
  pw:
    schema: {dir}
    value_env: {variable}
external_data_sources:
  bucket:
    schema: {dir}
    source_type: ObjectStorage
    location: https://storage.example.test/new/
    auth_method: NONE
  warehouse:
    schema: {dir}
    source_type: PostgreSQL
    location: pg.example.test:5432
    auth_method: BASIC
    options:
      DATABASE_NAME: app
      LOGIN: reader
      PASSWORD_SECRET_PATH: {dir}/pw
external_tables:
  events:
    schema: {dir}
    data_source: {dir}/bucket
    location: e/
    columns:
      - {name: id, type: Int64, not_null: true}
    options:
      FORMAT: json_each_row
views:
  v:
    name: {dir}.v
    body: "{query}"
`)

	c.Assert(hostPlanned(c, directory, "yaml", source), qt.DeepEquals, []string{
		"external-data-source-operation " + directory + ".bucket",
		"external-table-operation " + directory + ".stale",
		"external-data-source-operation " + directory + ".old",
		"external-table-operation " + directory + ".events",
		"secret-operation " + directory + ".pw",
		"external-data-source-operation " + directory + ".warehouse",
		"CREATE VIEW `" + directory + "/v` WITH (security_invoker = TRUE) AS",
	})
}
