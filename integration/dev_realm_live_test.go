//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// realmFixture names a dev realm in the test database and removes it, with the
// directory of the realms when nothing else is left in it, once the test ends.
func realmFixture(c *qt.C, opened *connection.Connection, base connection.Address) (address string, root string) {
	c.Helper()
	name := "r" + strings.ToLower(rand.Text())[:12]
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	query.Set("dev_realm", name)
	parsed.RawQuery = query.Encode()
	root = path.Join(base.Database(), connection.RealmDirectory, name)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, "records")+"`"), qt.IsNil)
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, root), qt.IsNil)
		// Another test may hold a realm of its own, which keeps the directory.
		_ = opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(base.Database(), connection.RealmDirectory))
	})
	return parsed.String(), root
}

func TestDevRealmSessionSubprocess(t *testing.T) {
	c := qt.New(t)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	realmAddress, root := realmFixture(c, opened, base)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)

	realm, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: realmAddress})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), realm.Id), qt.IsNil) })
	database, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), database.Id), qt.IsNil) })
	c.Assert(realm.Database, qt.Equals, root)
	c.Assert(database.Database, qt.Equals, base.Database())

	c.Run("a relative name lands in the realm", func(c *qt.C) {
		result := executeSQL(c, client, realm.Id, "", "CREATE TABLE `records` (id Uint64 NOT NULL, PRIMARY KEY (id))")
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
		assertRowCount(c, opened, path.Join(connection.RealmDirectory, path.Base(root), "records"), 0)
	})
	c.Run("the realm reads as a database of its own", func(c *qt.C) {
		snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: realm.Id})
		c.Assert(err, qt.IsNil)
		c.Assert(snapshot.Tables, qt.HasLen, 1)
		c.Assert(snapshot.Tables[0].Identity.GetName(), qt.Equals, "records")
		c.Assert(snapshot.Tables[0].Identity.GetSchema(), qt.Equals, "")
		c.Assert(snapshot.Tables[0].Identity.GetDatabase(), qt.Equals, root)
	})
	c.Run("the database leaves the realm out", func(c *qt.C) {
		snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: database.Id})
		c.Assert(err, qt.IsNil)
		for _, table := range snapshot.Tables {
			c.Assert(table.Identity.GetSchema(), qt.Not(qt.Equals), path.Join(connection.RealmDirectory, path.Base(root)))
			c.Assert(strings.HasPrefix(table.Identity.GetSchema(), connection.RealmDirectory), qt.IsFalse)
		}
		_, err = client.Inspect(ctx, &protocol.InspectRequest{SessionId: database.Id, Schemas: []string{connection.RealmDirectory}})
		c.Assert(err, qt.IsNotNil)
	})
	c.Run("an escape is refused before it runs", func(c *qt.C) {
		escape := path.Join(base.Database(), "ptah_escape_"+strings.ToLower(rand.Text())[:8])
		for _, statement := range []string{
			"CREATE TABLE `" + escape + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
			"CREATE TABLE `../ptah_escape_beside` (id Uint64 NOT NULL, PRIMARY KEY (id))",
			"PRAGMA TablePathPrefix = \"" + base.Database() + "\"",
		} {
			result := executeSQL(c, client, realm.Id, "", "SELECT 1", statement)
			c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
			c.Assert(result.DiagnosticCode, qt.Equals, "ydb.realm-escape")
			c.Assert(result.AppliedCount, qt.Equals, uint32(0))
		}
		_, err := opened.DB.ExecContext(ctx, "SELECT * FROM `"+escape+"`")
		c.Assert(err, qt.IsNotNil)
	})
}
