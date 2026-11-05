//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// testUser creates a user of the run's own, with no right of its own, and
// returns its name and the test address that authenticates as it. The cleanup
// drops the user. Such a user may connect: a default cluster's USERS group,
// which every user is in, holds ydb.database.connect on the database.
func testUser(c *qt.C, admin *connection.Connection, prefix string) (name, address string) {
	c.Helper()
	name, password := prefix+strings.ToLower(rand.Text())[:10], "Rights1"+rand.Text()
	c.Assert(execFixtureSchema(c.Context(), admin.DB, "CREATE USER `"+name+"` PASSWORD '"+password+"'"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, admin.DB, "DROP USER IF EXISTS `"+name+"`"), qt.IsNil)
	})
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	parsed.User = url.UserPassword(name, password)
	return name, parsed.String()
}

// openAs opens a native connection to address, closed by the cleanup.
func openAs(c *qt.C, address string) *connection.Connection {
	c.Helper()
	parsed, err := connection.Parse(address)
	c.Assert(err, qt.IsNil)
	opened, err := connection.Open(c.Context(), parsed)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	return opened
}

// tryLock takes the lock name on the lock node of database through driver
// and gives it back at once, and returns why it could not be taken.
func tryLock(c *qt.C, driver *ydb.Driver, database, name string) error {
	c.Helper()
	hold, busy, err := locking.Try(c.Context(), driver, database, name)
	if err != nil {
		return err
	}
	c.Assert(busy, qt.IsFalse)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return hold.Close(ctx)
}

// Ported from the host's TestYDBLock_TellsAUserWhatRightIsMissing: a user that
// may only connect is told, step by step, that the lock node is missing and
// that it may not create it, that it may not use the node another user
// created, and that the server refused it a session on the node it may only
// describe; once granted the node, it takes the lock, though it may not list
// the database root. The server refuses that session at once and says so, so
// the refusal names it where the host's embedded engine, whose SDK retries
// the refused session without a word, reports a timeout. The provider answers
// a refusal with a confirmed NOT_ACQUIRED and its reason, so the session is
// not fenced: once the user is granted the node, the same session takes the
// lock.
func TestNativeLockTellsAUserWhatRightIsMissing(t *testing.T) {
	c := testcheck.New(t)
	admin, _, database := lockFixture(c)
	name, address := testUser(c, admin, "ptahlocku")
	user := openAs(c, address)
	node := path.Join(database, locking.NodeName)
	grant := func(right string) {
		c.Helper()
		c.Assert(execFixtureSchema(c.Context(), admin.DB, "GRANT "+right+" ON `"+node+"` TO `"+name+"`"), qt.IsNil)
	}
	client := startProviderFor(c, provider.ErrorReasonsFeature)
	// YDB 26.2 refuses its feature flags page to a user with no right of its
	// own, and a session that names no monitoring endpoint takes its
	// capabilities from the server version, which is all a lock needs.
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: withoutMonitoring(c, address)})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	refusal, refusedThroughProvider := client.TryLock(c.Context(), nativeLockRequest(session.Id, "refused", "ptah_lock_rights"))
	noNode := tryLock(c, user.Driver, database, "ptah_lock_rights")
	created := tryLock(c, admin.Driver, database, "ptah_lock_rights")
	noAccess := tryLock(c, user.Driver, database, "ptah_lock_rights")
	grant("DESCRIBE SCHEMA")
	noSession := tryLock(c, user.Driver, database, "ptah_lock_rights")
	grant("ALL")
	taken := tryLock(c, user.Driver, database, "ptah_lock_rights")
	granted, grantedErr := client.TryLock(c.Context(), nativeLockRequest(session.Id, "granted", "ptah_lock_rights"))

	quoted := regexp.QuoteMeta(node)
	c.Assert(refusedThroughProvider, qt.ErrorIs, provider.ErrExecution)
	c.Assert(refusedThroughProvider, qt.Not(qt.ErrorIs), provider.ErrOutcomeUnknown)
	c.Assert(refusedThroughProvider, qt.ErrorMatches, `provider lock outcome is NOT_ACQUIRED: Ptah's locks live on the YDB coordination node `+
		quoted+`, which does not exist, and this user may not create it \(UNAUTHORIZED\)\. .*`)
	c.Assert(refusal.GetOutcome(), qt.Equals, protocol.LockResult_NOT_ACQUIRED)
	c.Assert(grantedErr, qt.IsNil)
	c.Assert(granted.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(noNode, qt.ErrorIs, locking.ErrRefused)
	c.Assert(noNode, qt.ErrorMatches, `Ptah's locks live on the YDB coordination node `+quoted+`, which does not exist, and this user may not `+
		`create it \(UNAUTHORIZED\)\. Run once as a user who may create a coordination node at the database root, which creates it, `+
		`and grant this user the right to use it`)
	c.Assert(created, qt.IsNil)
	c.Assert(noAccess, qt.ErrorIs, locking.ErrRefused)
	c.Assert(noAccess, qt.ErrorMatches, `this user may not use the YDB coordination node `+quoted+`, which holds Ptah's locks \(UNAUTHORIZED\); `+
		`grant the user the right to use it`)
	c.Assert(noSession, qt.ErrorIs, locking.ErrRefused)
	c.Assert(noSession, qt.ErrorMatches, `open a session on the YDB coordination node `+quoted+`, which holds Ptah's locks: the server refused it `+
		`\(UNAUTHORIZED\); grant the user the right to use it`)
	c.Assert(taken, qt.IsNil)
}
