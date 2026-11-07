//go:build integration

package integration_test

import (
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host's former TestOnlyAFormatThatDeclaresSecretsClaimsTheirNamespace
// requires a source format that can declare YDB secrets to claim that
// namespace. YAML and YQL must plan the removal of a held secret they omit.
// HCL and DBML cannot declare secrets and must preserve that same object.
func TestHostOnlySecretDeclaringFormatsClaimSecretNamespace(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	absolute := path.Join(database, directory, "held")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE SECRET `"+absolute+"` WITH (value = 'probe');"), qt.IsNil)
	secret := "secret-operation " + directory + ".held"
	table := "row-table-create-operation " + directory + ".other"

	rows := []struct {
		name   string
		format string
		text   string
		want   []string
	}{
		{name: "YAML", format: "yaml", text: "secrets: {}\n", want: []string{secret}},
		{name: "YQL", format: "sql", text: "CREATE TABLE `" + directory + "/other` (id Int64 NOT NULL, PRIMARY KEY (id));\n", want: []string{secret, table}},
		{name: "HCL", format: "hcl", text: `schema "` + directory + `" {}
table "other" {
  schema = schema.` + directory + `
  column "id" { type = Int64 }
  primary_key { columns = [column.id] }
}
`, want: []string{table}},
		{name: "DBML", format: "dbml", text: "Table " + directory + ".other {\n  id integer [pk]\n}\n", want: []string{table}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			planned := hostPlanned(c, directory, row.format, row.text)
			slices.Sort(planned)
			slices.Sort(row.want)
			c.Assert(planned, qt.DeepEquals, row.want)
		})
	}
}
