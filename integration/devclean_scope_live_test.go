//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// emptyRealmOnCleanup empties the session's realm when the test ends, so the
// realm fixture can remove its directory whatever the test left in it.
func emptyRealmOnCleanup(c *qt.C, client *providerruntime.Client, session string) {
	c.Helper()
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleaned, err := client.CleanDevDatabase(cleanup, &protocol.CleanDevRequest{SessionId: session, RequestId: "fixture"})
		c.Check(err, qt.IsNil)
		c.Check(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED)
	})
}

// A dev cleanup scoped to one directory removes everything in it, at any
// depth: row and column tables, a changefeed, a view, a topic with a consumer,
// a coordination node and the nested directories. The directory beside it and
// what that holds stay. This is how a caller empties a directory it made for
// itself, which nothing else in YQL can do.
func TestDevCleanupOfADirectoryLeavesItsSiblingSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	for _, statement := range []string{
		"CREATE TABLE `probe/t1` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))",
		"ALTER TABLE `probe/t1` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"CREATE TABLE `probe/deeper/t2` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))",
		"CREATE TABLE `probe/olap` (`id` Int64 NOT NULL, PRIMARY KEY (`id`)) PARTITION BY HASH(`id`) WITH (STORE = COLUMN)",
		"CREATE VIEW `probe/v` WITH (security_invoker = TRUE) AS SELECT `id` FROM `probe/t1`",
		"CREATE TOPIC `probe/deeper/events` (CONSUMER `reader`)",
		"CREATE COORDINATION NODE `probe/deeper/locks`",
		"CREATE TABLE `keep/t3` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))",
	} {
		result := executeSQL(c, client, session, "", statement)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s: %s", statement, result.Message))
	}

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "probe", Schemas: []string{"probe"}})

	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	c.Assert(listedObjects(c, client, session, "probe"), qt.HasLen, 0)
	c.Assert(childNames(c, opened, path.Join(root, "probe")), qt.HasLen, 0)
	c.Assert(childNames(c, opened, root), qt.Contains, "keep")
	c.Assert(childNames(c, opened, path.Join(root, "keep")), qt.DeepEquals, []string{"t3"})
}

// A dev cleanup names and drops the migrator's own tables in every directory
// that holds one: a revision table at the root and another in a directory,
// and the tag table beside it. Inspection reports them as reserved for the
// migrator rather than as tables a schema declares, and the cleanup still
// lists them with the user's table and leaves the realm empty. The read
// selects no directory, so on a server with streaming queries it also takes
// the database-wide streaming query inventory, which must keep what the
// reservations claim.
func TestDevCleanupNamesTheMigratorsTablesSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	emptyRealmOnCleanup(c, client, session)
	c.Assert(executeSQL(c, client, session, "", "CREATE TABLE `users` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))").Outcome, qt.Equals, protocol.Execution_APPLIED)
	for _, scope := range []*protocol.HistoryScope{
		{SessionId: session, Table: "schema_migrations", Format: protocol.HistoryScope_NATIVE},
		{SessionId: session, Schema: "app", Table: "schema_migrations", Format: protocol.HistoryScope_NATIVE},
	} {
		prepared, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare " + scope.Schema})
		c.Assert(err, qt.IsNil)
		c.Assert(prepared.GetOutcome(), qt.Equals, protocol.HistoryResult_APPLIED)
	}
	tags, err := client.PrepareMetadata(c.Context(), &protocol.PrepareMetadataRequest{RequestId: "tags",
		Scope: &protocol.MetadataScope{SessionId: session, Schema: "app", Table: "ptah_migration_tags", Kind: protocol.MetadataScope_TAGS}})
	c.Assert(err, qt.IsNil)
	c.Assert(tags.GetOutcome(), qt.Equals, protocol.MetadataResult_APPLIED)

	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session})
	c.Assert(err, qt.IsNil)
	listed := listedObjects(c, client, session)
	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session, RequestId: "clean"})

	var tables, reserved []string
	for _, table := range snapshot.GetTables() {
		tables = append(tables, path.Join(table.GetIdentity().GetSchema(), table.GetIdentity().GetName()))
	}
	for _, object := range snapshot.GetReservedObjects() {
		reserved = append(reserved, path.Join(object.GetObject().GetSchema(), object.GetObject().GetName()))
	}
	slices.Sort(reserved)
	c.Assert(tables, qt.DeepEquals, []string{"users"})
	c.Assert(reserved, qt.DeepEquals, []string{"app/ptah_migration_tags", "app/schema_migrations", "schema_migrations"})
	c.Assert(listed, qt.DeepEquals, []string{
		"table app/ptah_migration_tags", "table app/schema_migrations", "directory app", "table schema_migrations", "table users",
	})
	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED, qt.Commentf("%s", cleaned.GetMessage()))
	c.Assert(listedObjects(c, client, session), qt.HasLen, 0)
	c.Assert(childNames(c, opened, root), qt.HasLen, 0)
}
