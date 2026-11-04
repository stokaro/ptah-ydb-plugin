//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// testAddress is the test URL with the query parameter given replaced, or
// with the user given, for a session the provider opens.
func testAddress(c *qt.C, user *url.Userinfo, parameter, value string) string {
	c.Helper()
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	c.Assert(query.Get("monitoring"), qt.Not(qt.Equals), "", qt.Commentf("the test URL names no monitoring endpoint"))
	if parameter != "" {
		query.Set(parameter, value)
	}
	parsed.RawQuery, parsed.User = query.Encode(), user
	return parsed.String()
}

// serverVersion is the version of the server the test URL names.
func serverVersion(c *qt.C, opened *connection.Connection) string {
	c.Helper()
	var version string
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT Version()").Scan(&version), qt.IsNil)
	return version
}

// connectingUser creates a user with a password and the rights given on the
// database, and drops it when the test ends. Users belong to the whole
// cluster, so the name is the test's own.
func connectingUser(c *qt.C, opened *connection.Connection, database string, rights ...string) (string, string) {
	c.Helper()
	user, password := "ptahflags"+strings.ToLower(rand.Text()[:10]), "Pw"+rand.Text()
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE USER "+user+" PASSWORD '"+password+"'"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS "+user), qt.IsNil)
	})
	for _, right := range rights {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "GRANT "+right+" ON `"+database+"` TO "+user), qt.IsNil)
	}
	return user, password
}

// Ported from the host's TestYDBConnection_ReadsTheClusterFeatureFlags_FailurePath:
// a monitoring endpoint that does not answer fails the session, since a plan
// made without the cluster's flags would be made for a cluster nobody
// described. Only the port moves, to one nothing listens on. The host names
// the endpoint in its refusal; the provider keeps addresses out of what it
// says, so its refusal names neither the endpoint nor the port.
func TestSessionNeedsAnAnsweringMonitoringEndpoint_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	monitoring, err := url.Parse(parsed.Query().Get("monitoring"))
	c.Assert(err, qt.IsNil)
	silent := "http://" + net.JoinHostPort(monitoring.Hostname(), "1")
	client := startProviderFor(c)

	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: testAddress(c, nil, "monitoring", silent)})

	c.Assert(session, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
	c.Assert(err.Error(), qt.Not(qt.Contains), net.JoinHostPort(monitoring.Hostname(), "1"))
}

// Ported from the host's TestYDBConnection_ReadsTheFlagsAsTheConnectingUser_HappyPath:
// the flags page is read as the user the URL connects as, and a user who may
// describe the database reads it on both lines and gets the capabilities an
// administrator's session gets; 25.1 serves it to a user who may only
// connect, because that line's endpoint checks no right beyond the login.
func TestSessionReadsTheFlagsAsTheConnectingUser_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		before26 bool
		rights   []string
	}{
		{name: "a user who may describe the database, on a 26.x line", rights: []string{"CONNECT", "DESCRIBE SCHEMA"}},
		{name: "a user who may describe the database, on a 25.x line", before26: true, rights: []string{"CONNECT", "DESCRIBE SCHEMA"}},
		{name: "a user who may only connect, on a 25.x line", before26: true, rights: []string{"CONNECT"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, _, database := inspectionFixture(c)
			skipOnLine(c, test.before26, serverVersion(c, opened))
			user, password := connectingUser(c, opened, database, test.rights...)
			client := startProviderFor(c)
			admin, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: testAddress(c, nil, "", "")})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), admin.Id), qt.IsNil) })

			session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: testAddress(c, url.UserPassword(user, password), "", "")})

			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
			c.Assert(session.Capabilities, qt.DeepEquals, admin.Capabilities)
		})
	}
}

// skipOnLine skips a row on the line it does not describe: one meant for a
// 25.x line on a later one, and one meant for a 26.x line on an earlier one.
// The two lines' viewers check different rights, as measured by the host on
// 25.1.4.7 and 26.2.1.14.
func skipOnLine(c *qt.C, before26 bool, version string) {
	c.Helper()
	if before26 == releaseline.AtLeast(version, 26, 0) {
		c.Skipf("the row describes another release line than %s", version)
	}
}

// Ported from the host's TestYDBConnection_ReadsTheFlagsAsTheConnectingUser_FailurePath:
// on a 26.x line, a user who may only connect cannot read the flags page,
// so the session is refused with the right it needs, and the refusal repeats
// neither the password nor the endpoint.
func TestSessionReadsTheFlagsAsTheConnectingUser_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, database := inspectionFixture(c)
	skipOnLine(c, false, serverVersion(c, opened))
	user, password := connectingUser(c, opened, database, "CONNECT")
	client := startProviderFor(c)

	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: testAddress(c, url.UserPassword(user, password), "", "")})

	c.Assert(session, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, `(?s).*needs DESCRIBE SCHEMA on `+database+`.*`)
	c.Assert(err.Error(), qt.Not(qt.Contains), password)
}
