//go:build integration

package integration_test

import (
	"fmt"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBWriter_DropAllTablesDropsReplicationsFirst: a
// dev cleanup of a realm that holds a running replication and the replica a
// replication dropped without CASCADE left behind drops the replication before
// the tables, and then every table, the replicas among them. The host's engine
// leaves the orphaned replica, which its reader records rather than
// describes; the provider's cleanup walks the scheme tree, so it drops the
// replica as it drops any other table and leaves the realm empty.
func TestDevCleanupDropsReplicationsFirstSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	connection := selfConnection(c, opened)
	at := func(name string) string { return path.Join(root, name) }
	for _, statement := range []string{
		"CREATE TABLE `" + at("src") + "` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))",
		fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s')", at("mirror"), at("src"), at("rep"), connection),
		fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s')", at("orphaning"), at("src"), at("orphan"), connection),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	awaitNames(c, opened, root, []string{"mirror", "orphan", "orphaning", "rep", "src"})
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "DROP ASYNC REPLICATION `"+at("orphaning")+"`"), qt.IsNil)
	awaitNames(c, opened, root, []string{"mirror", "orphan", "rep", "src"})

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "clean"})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	c.Assert(childNames(c, opened, root), qt.HasLen, 0)
}

// Ported from the host's TestYDBWriter_DropDirectoryDropsReplicationsFirst: a
// dev cleanup of a directory that holds a running replication and its replica
// drops the replication first, without CASCADE, and then the replica, which
// YDB drops for a table it keeps read-only. The source table beside the
// directory stays.
func TestDevCleanupOfADirectoryDropsItsReplicationFirstSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	at := func(name string) string { return path.Join(root, name) }
	for _, statement := range []string{
		"CREATE TABLE `" + at("src") + "` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))",
		fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s')", at("probe/mirror"), at("src"), at("probe/rep"), selfConnection(c, opened)),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	awaitNames(c, opened, at("probe"), []string{"mirror", "rep"})

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "probe", Schemas: []string{"probe"}})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	c.Assert(childNames(c, opened, at("probe")), qt.HasLen, 0)
	c.Assert(childNames(c, opened, root), qt.Contains, "src")
}
