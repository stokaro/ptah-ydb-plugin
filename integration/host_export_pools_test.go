//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host's built-in engine wrote the database's resource pools into a Go
// export, of the whole database and of one directory alike:
// `//ptah:schema:resourcepool name="default"`. A read that knows every native
// kind completely leaves no kind narrowed but the table facet kinds, which no
// source limit can say; the provider claims those complete for each directory
// it lists in full, so the host can write the export. Without that claim the
// host's encoding is refused, `provider encode source failed (Internal)`.
func TestHostIntrospectOfTheDatabaseWritesItsPools_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	items := path.Join(database, directory, "items")
	createSchemaObject(c, opened, "CREATE TABLE `"+items+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+items+"`")
	out := c.TempDir()

	hostExport(c, "introspect", "--out", out, "--single-file")

	written, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	source := string(written)
	c.Assert(source, qt.Contains, `//ptah:schema:resourcepool name="default"`, qt.Commentf("%s", source))
	c.Assert(source, qt.Not(qt.Contains), `kind="resource_pool`, qt.Commentf("%s", source))
	c.Assert(source, qt.Contains, `name="items"`, qt.Commentf("%s", source))
}

// A read of one directory reports the database's pools beside its scope where
// the host lists schema.database-kinds (ADR 0065), and its Go export writes
// them as the built-in engine's did.
func TestHostIntrospectOfADirectoryWritesTheDatabasePools_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DatabaseKindsFeature)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	items := path.Join(database, directory, "items")
	createSchemaObject(c, opened, "CREATE TABLE `"+items+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+items+"`")

	source := introspectDirectory(c, directory)

	c.Assert(source, qt.Contains, `//ptah:schema:resourcepool name="default"`, qt.Commentf("%s", source))
	c.Assert(source, qt.Not(qt.Contains), `kind="resource_pool`, qt.Commentf("%s", source))
	c.Assert(source, qt.Contains, `name="items"`, qt.Commentf("%s", source))
}
