//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

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

// Ported from TestRender_ResourcePool_AloneKeepsItsStruct. A Go export of
// an otherwise empty directory still needs the struct that owns standalone
// annotations. First the database's default pool, then a classifier on that
// pool, must be attached to that struct without a table declaration. A live
// YDB read always includes the default pool, so the classifier is not its
// only global object as it was in the old synthetic host test.
func TestHostGoExportKeepsTheStructForOnlyWorkloadObjects_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DatabaseKindsFeature)
	opened, directory, _ := lockFixture(c)
	enableResourcePools(c, opened)
	assertWorkloadOnly := func(source string, annotations ...string) {
		c.Helper()
		c.Assert(source, qt.Contains, "type PtahSchemaObjects struct{}", qt.Commentf("%s", source))
		c.Assert(source, qt.Not(qt.Contains), "//ptah:schema:table", qt.Commentf("%s", source))
		for _, annotation := range annotations {
			c.Assert(source, qt.Contains, annotation, qt.Commentf("%s", source))
			c.Assert(strings.Index(source, annotation) < strings.Index(source, "type PtahSchemaObjects struct{}"), qt.IsTrue)
		}
	}

	poolOnly := introspectDirectory(c, directory)
	assertWorkloadOnly(poolOnly, `//ptah:schema:resourcepool name="default"`)

	classifier := directory + "_route"
	rank, _ := workloadRanks(directory)
	createSchemaObject(c, opened,
		"CREATE RESOURCE POOL CLASSIFIER `"+classifier+"` WITH (RESOURCE_POOL = 'default', MEMBER_NAME = '"+workloadMember(directory)+"', RANK = "+strconv.FormatInt(rank, 10)+")",
		"DROP RESOURCE POOL CLASSIFIER `"+classifier+"`")
	withClassifier := introspectDirectory(c, directory)
	assertWorkloadOnly(withClassifier, `//ptah:schema:resourcepool name="default"`,
		`//ptah:schema:resourcepool:classifier name="`+classifier+`"`)
}
