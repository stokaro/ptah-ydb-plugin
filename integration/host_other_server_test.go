//go:build integration

package integration_test

import (
	"context"
	"os"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// otherServerVariable names a second YDB server, for the tests whose dev
// database must be on a server other than the target's. Both servers name
// their database /local, so the database path cannot tell the two addresses
// apart, and the host cannot either: one YDB database answers on every node of
// its cluster.
const otherServerVariable = "PTAH_YDB_OTHER_TEST_URL"

// otherServer opens the second server and returns its address and connection,
// or skips the test where none is configured.
func otherServer(c *qt.C) (string, *connection.Connection) {
	c.Helper()
	raw := os.Getenv(otherServerVariable)
	if raw == "" {
		c.Skipf("%s names no second YDB server", otherServerVariable)
	}
	address, err := connection.Parse(raw)
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	return raw, opened
}
