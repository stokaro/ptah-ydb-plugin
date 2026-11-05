//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// accessSession opens a provider session on the test address for a host that
// carries common objects and access, and offers object context, which a read
// of a streaming query needs, closed by the cleanup.
func accessSession(c *qt.C) (*providerruntime.Client, *protocol.Session) {
	c.Helper()
	client := startProviderFor(c, provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningObjectContextFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return client, session
}

// skipWithout skips the test on a target whose session does not establish
// the capability key.
func skipWithout(c *qt.C, session *protocol.Session, key string) {
	c.Helper()
	if !session.Capabilities[key] {
		c.Skipf("the target does not establish %s", key)
	}
}

// skipWith skips the test on a target whose session establishes the
// capability key.
func skipWith(c *qt.C, session *protocol.Session, key string) {
	c.Helper()
	if session.Capabilities[key] {
		c.Skipf("the target establishes %s", key)
	}
}

// grantEntry is one permission a read reports a principal holds on one
// object.
type grantEntry struct {
	Role, Privilege, Kind, Name string
	Implicit                    bool
}

// grantsIn is every permission a read of directory reports on the objects of
// the given grant kinds, for the roles given, sorted.
func grantsIn(c *qt.C, client *providerruntime.Client, session *protocol.Session, directory string, kinds, roles []string) []grantEntry {
	c.Helper()
	return grantsInSchemas(c, client, session, []string{directory}, kinds, roles)
}

// grantsInSchemas is grantsIn over a read of the directories given; / is
// the database root, whose read also reports the resource pools.
func grantsInSchemas(c *qt.C, client *providerruntime.Client, session *protocol.Session, schemas, kinds, roles []string) []grantEntry {
	c.Helper()
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: schemas})
	c.Assert(err, qt.IsNil)
	var entries []grantEntry
	for _, grant := range snapshot.GetCommonObjects().GetGrants() {
		target := grant.GetTarget()
		if slices.Contains(kinds, target.GetKind()) && slices.Contains(roles, grant.GetRole()) {
			entries = append(entries, grantEntry{grant.GetRole(), grant.GetPrivilege(), target.GetKind(), path.Join(target.GetSchema(), target.GetName()), grant.GetImplicit()})
		}
	}
	sortGrantEntries(entries)
	return entries
}

// principalName is a user or group name of its own for one run, which YDB
// takes: lower-case letters and digits. Principals belong to the whole
// database, so a run that shared a name would inherit what another left.
func principalName(prefix string) string {
	return prefix + strings.ToLower(rand.Text())[:10]
}

// Ported from the host's TestYDBObjectAccess_AnAsyncReplication: the plan
// that creates an async replication of the server's own table grants a user
// a permission on it, after it creates the replication; the read reports the
// entry and nothing is left to plan. A source that no longer declares the
// replication drops it with its replica, which takes the entry, and writes no
// REVOKE, since a REVOKE after the drop would name a path that is gone.
//
// The source table is applied first, as TestHostAppliesAnAsyncReplication_HappyPath
// applies it: the replication planner does not order a creation after the
// creation of a table it replicates, and a replication created before its
// source never makes its replica. The host's engine plans both in one run.
func TestHostGrantsOnAnAsyncReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	user := principalName("ptahrepu")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		absolute := func(name string) string { return path.Join(database, directory, name) }
		for _, statement := range []string{"DROP ASYNC REPLICATION `" + absolute("mirror") + "` CASCADE", "DROP TABLE `" + absolute("rep") + "`", "DROP TABLE `" + absolute("src") + "`"} {
			_ = execFixtureSchema(cleanup, opened.DB, statement)
		}
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	base := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n" +
		"roles:\n  " + user + ":\n    login: true\n"
	declared := base + "async_replications:\n  mirror:\n    schema: " + directory + "\n    connection_string: " + strconv.Quote(selfConnection(c, opened)) +
		"\n    items:\n      - source: " + path.Join(database, directory, "src") + "\n        target: " + path.Join(directory, "rep") + "\n" +
		"grants:\n  reader:\n    role: " + user + "\n    privileges: [ydb.generic.read]\n    on_async_replication: " + directory + ".mirror\n"
	emptied := base + "async_replications: {}\n"
	grant := fmt.Sprintf("GRANT 'ydb.generic.read' ON `%s/mirror` TO `%s`", directory, user)

	hostRun(c, directory, "the table", strings.SplitN(base, "roles:", 2)[0], `(?s).*Schema apply completed successfully.*`)
	first := hostPlanned(c, directory, "yaml", declared)
	hostRun(c, directory, "the replication", declared, `(?s).*Schema apply completed successfully.*`)
	awaitNames(c, opened, path.Join(database, directory), []string{"mirror", "rep", "src"})
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
	granted := grantsIn(c, client, session, directory, []string{"async replication"}, []string{user})
	removal := hostPlanned(c, directory, "yaml", emptied)
	hostRun(c, directory, "the removal", emptied, `(?s).*Schema apply completed successfully.*`)
	awaitNames(c, opened, path.Join(database, directory), []string{"src"})

	c.Assert(first, qt.Contains, "async-replication-operation "+directory+".mirror")
	c.Assert(first[len(first)-1], qt.Equals, grant)
	c.Assert(granted, qt.DeepEquals, []grantEntry{{Role: user, Privilege: "ydb.generic.read", Kind: "async replication", Name: directory + "/mirror"}})
	c.Assert(removal, qt.DeepEquals, []string{"async-replication-operation " + directory + ".mirror"})
	hostRun(c, directory, "a run after the removal", emptied, `(?s).*Schema is synced, no changes to be made.*`)
}

// Ported from the host's TestYDBObjectAccess_ATransfer: the plan that creates
// a transfer grants a user a permission on it, after it creates the transfer;
// the read reports the entry and nothing is left to plan. A changed lambda,
// which YDB changes in place, keeps the entry, and the plan is the transfer's
// one change. A source that no longer declares the transfer drops it without
// a REVOKE. The transfer reads through a consumer the changefeed declares, as
// TestHostAppliesATransfer_HappyPath does, because the provider does not adopt
// the consumer YDB would create. The tables and the changefeed are applied
// first, as that test applies them: the transfer planner orders a creation
// after its table but not after the changefeed whose topic it reads, which
// another planning service adds.
func TestHostGrantsOnATransfer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	skipWithout(c, session, replication.CapabilityTransfers)
	user := principalName("ptahtrnu")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TRANSFER `"+path.Join(database, directory, "ingest")+"`")
		for _, name := range []string{"order_log", "orders"} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+path.Join(database, directory, name)+"`")
		}
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	base := "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"    changefeeds:\n      feed:\n        mode: NEW_IMAGE\n        format: JSON\n        consumers:\n          reader: {}\n" +
		"  order_log:\n    schema: " + directory + "\n    columns:\n      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n" +
		"roles:\n  " + user + ":\n    login: true\n"
	declared := func(prefix string) string {
		return base + "transfers:\n  ingest:\n    schema: " + directory + "\n    source: " + path.Join(directory, "orders/feed") + "\n    target: " + path.Join(directory, "order_log") +
			"\n    consumer: reader\n    flush_interval: PT1S\n    using: " + strconv.Quote(offsetLambda(prefix)) + "\n" +
			"grants:\n  manager:\n    role: " + user + "\n    privileges: [ydb.generic.manage]\n    on_transfer: " + directory + ".ingest\n"
	}
	emptied := base + "transfers: {}\n"
	operation := "transfer-operation " + directory + ".ingest"
	want := []grantEntry{{Role: user, Privilege: "ydb.generic.manage", Kind: "transfer", Name: directory + "/ingest"}}
	read := func() []grantEntry {
		c.Helper()
		return grantsIn(c, client, session, directory, []string{"transfer"}, []string{user})
	}

	hostRun(c, directory, "the tables", strings.SplitN(base, "roles:", 2)[0], `(?s).*Schema apply completed successfully.*`)
	first := hostPlanned(c, directory, "yaml", declared("a:"))
	hostRun(c, directory, "the transfer", declared("a:"), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a second run", declared("a:"), `(?s).*Schema is synced, no changes to be made.*`)
	created := read()
	relambda := hostPlanned(c, directory, "yaml", declared("b:"))
	hostRun(c, directory, "a new lambda", declared("b:"), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a run after the change", declared("b:"), `(?s).*Schema is synced, no changes to be made.*`)
	kept := read()
	removal := hostPlanned(c, directory, "yaml", emptied)
	hostRun(c, directory, "the removal", emptied, `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a run after the removal", emptied, `(?s).*Schema is synced, no changes to be made.*`)

	c.Assert(first, qt.Contains, operation)
	c.Assert(first[len(first)-1], qt.Equals, fmt.Sprintf("GRANT 'ydb.generic.manage' ON `%s/ingest` TO `%s`", directory, user))
	c.Assert(created, qt.DeepEquals, want)
	c.Assert(relambda, qt.DeepEquals, []string{operation})
	c.Assert(kept, qt.DeepEquals, want)
	c.Assert(removal, qt.DeepEquals, []string{operation})
	c.Assert(read(), qt.HasLen, 0)
}
