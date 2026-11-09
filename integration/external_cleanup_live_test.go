//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// externalCleanupFixture creates an external data source in a/ and an external
// table in z/ that reads it, below root, so a walk of root meets the source
// before the table that depends on it.
func externalCleanupFixture(c *qt.C, root string, exec func(string) error) {
	c.Helper()
	at := func(name string) string { return path.Join(root, name) }
	for _, statement := range []string{
		"CREATE EXTERNAL DATA SOURCE `" + at("a/source") + "` WITH (SOURCE_TYPE = 'ObjectStorage', " +
			"LOCATION = 'https://storage.invalid/b/', AUTH_METHOD = 'NONE')",
		"CREATE EXTERNAL TABLE `" + at("z/files") + "` (id Int64) WITH (DATA_SOURCE = '" + at("a/source") + "', " +
			"LOCATION = 'f/', FORMAT = 'json_each_row')",
	} {
		c.Assert(exec(statement), qt.IsNil, qt.Commentf("%s", statement))
	}
}

// Ported from the host's TestYDBExternal_TeardownsDropThem (DropAllTables): a
// dev cleanup of a realm whose walk meets an external data source before the
// external table that reads it drops the table first, as YDB refuses to drop
// a source a table still names, and leaves the realm empty.
func TestDevCleanupDropsAnExternalTableBeforeItsSourceSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	externalCleanupFixture(c, root, func(statement string) error { return execFixtureSchema(c.Context(), opened.DB, statement) })
	c.Assert(childNames(c, opened, root), qt.DeepEquals, []string{"a", "z"})

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "clean"})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	c.Assert(childNames(c, opened, root), qt.HasLen, 0)
}

// Ported from the host's TestYDBExternal_TeardownsDropThem (DropDirectory): a
// dev cleanup of the table's directory drops the external table and leaves
// the data source in its own directory; a cleanup of that directory then drops
// the source. A cleanup of the directory above both drops the table before
// the source its walk meets first.
func TestDevCleanupOfADirectoryDropsExternalObjectsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	exec := func(statement string) error { return execFixtureSchema(c.Context(), opened.DB, statement) }
	clean := func(schemas ...string) {
		c.Helper()
		cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "clean", Schemas: schemas})
		c.Assert(err, qt.IsNil)
		c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	}

	externalCleanupFixture(c, path.Join(root, "each"), exec)
	clean("each/z")
	c.Assert(childNames(c, opened, path.Join(root, "each", "z")), qt.HasLen, 0)
	c.Assert(childNames(c, opened, path.Join(root, "each", "a")), qt.DeepEquals, []string{"source"})
	clean("each/a")
	c.Assert(childNames(c, opened, path.Join(root, "each", "a")), qt.HasLen, 0)

	externalCleanupFixture(c, path.Join(root, "both"), exec)
	clean("both")
	c.Assert(childNames(c, opened, path.Join(root, "both")), qt.HasLen, 0)
}
