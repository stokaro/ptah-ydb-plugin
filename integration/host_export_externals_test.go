//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `ptah introspect` writes a directory's data source and the external table
// over it as the directives the built-in engine wrote, and the export,
// applied over the directory, plans nothing. The provider compared the bytes
// of what each directive decodes to with the read, and the read holds what
// no declaration writes: 26.2 lists the tables over a source as its
// REFERENCES, and a read states a NOT NULL column by a type that is not
// Optional alone. So the host refused the export on both lines (`Go
// annotations cannot represent a value of provider ...: the source grammar
// cannot preserve every setting of this declaration`).
func TestHostExportsExternalObjectsAsGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	source, table := path.Join(database, directory, "store"), path.Join(database, directory, "events")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL TABLE `"+table+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL DATA SOURCE `"+source+"`")
	})
	for _, statement := range []string{
		"CREATE EXTERNAL DATA SOURCE `" + source + "` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.com/bucket/', AUTH_METHOD = 'NONE')",
		"CREATE EXTERNAL TABLE `" + table + "` (id Uint64 NOT NULL, body Utf8) WITH (DATA_SOURCE = '" + source + "', LOCATION = '/data/', FORMAT = 'json_each_row')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	written := introspectDirectory(c, directory)
	replanned, err := hostApplySource(c, directory, "go", written)

	c.Assert(written, qt.Contains, `//ptah:schema:externaldatasource name="store" schema="`+directory+
		`" source_type="ObjectStorage" location="https://storage.example.com/bucket/" auth_method="NONE"`)
	c.Assert(written, qt.Contains, `//ptah:schema:externaltable name="events" schema="`+directory+`" data_source="`+directory+
		`/store" location="/data/" columns="id Uint64 NOT NULL, body Utf8" options="FORMAT=json_each_row"`)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", written, replanned))
	c.Assert(replanned, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", written, replanned))
}
