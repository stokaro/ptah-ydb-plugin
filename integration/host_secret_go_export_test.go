//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A secret's value cannot be read, but its path must survive a Go export as a
// declaration using the default environment variable. Applying that export
// back must not rotate or remove either a root or a nested secret.
func TestHostGoExportCarriesSecretsByDefaultVariable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	nested := path.Join(database, directory, "ext", "aws")
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(database, directory, "ext")), qt.IsNil)
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), nested), qt.IsNil)
	for _, absolute := range []string{
		path.Join(database, directory, "pg_password"),
		path.Join(nested, "s3-key"),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE SECRET `"+absolute+"` WITH (value = 'probe');"), qt.IsNil)
	}

	selectors := directory + "," + path.Join(directory, "ext", "aws")
	out := c.TempDir()
	hostExport(c, "introspect", "--schemas", selectors, "--out", out, "--single-file")
	written, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	source := string(written)
	for _, secret := range []struct{ schema, name string }{
		{directory, "pg_password"},
		{path.Join(directory, "ext", "aws"), "s3-key"},
	} {
		variable := "PTAH_SECRET_" + strings.ToUpper(strings.NewReplacer("/", "_", "-", "_").Replace(secret.schema+"/"+secret.name))
		c.Assert(source, qt.Contains, `//ptah:schema:secret name="`+secret.name+`" schema="`+secret.schema+
			`" value_env="`+variable+`"`)
	}
	c.Assert(hostPlanned(c, selectors, "go", source), qt.HasLen, 0)
}
