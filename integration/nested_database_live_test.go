//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// nestedTable creates the table items in a directory of its own in the
// database other, which a read that descended into it would report.
func nestedTable(c *qt.C, other *connection.Connection) {
	c.Helper()
	directory := "ptah4204_" + strings.ToLower(rand.Text())
	absolute := path.Join(other.Driver.Name(), directory)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, other.DB, "CREATE TABLE `"+absolute+"/items` (id Uint64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, other.DB, "DROP TABLE `"+absolute+"/items`"), qt.IsNil)
		c.Check(other.Driver.Scheme().RemoveDirectory(cleanup, absolute), qt.IsNil)
	})
}

// readBelow lists the tables and native objects of snapshot whose directory is
// directory or lies below it, as directory|name.
func readBelow(snapshot *protocol.Schema, directory string) []string {
	var read []string
	below := func(schema string) bool { return schema == directory || strings.HasPrefix(schema, directory+"/") }
	for _, table := range snapshot.GetTables() {
		if below(table.GetIdentity().GetSchema()) {
			read = append(read, table.GetIdentity().GetSchema()+"|"+table.GetIdentity().GetName())
		}
	}
	for _, object := range snapshot.GetNativeObjects() {
		if below(object.GetOwner().GetSchema()) {
			read = append(read, object.GetOwner().GetSchema()+"|"+object.GetOwner().GetName())
		}
	}
	return read
}

// A database whose root sits below the test database's, such as /local/db2
// below /local, is another database. A read of the whole test database leaves
// it and what it holds out and still reads the test database's own tables, as
// the host's embedded engine at 74de0b65b read them on 25.1.4.7 and 26.2.1.14.
// Read as an object, it refused the whole database (`YDB object kind DATABASE
// needs an extracted reader`, which the host printed as `provider inspect
// failed (FailedPrecondition)`), and so did every apply, rollback and
// introspection on a server that held one.
func TestRootReadLeavesANestedDatabaseOutSubprocess(t *testing.T) {
	c := testcheck.New(t)
	_, other := otherDatabase(c)
	opened, directory, database := inspectionFixture(c)
	nested := other.Driver.Name()
	c.Assert(path.Dir(nested), qt.Equals, database, qt.Commentf("%s names a database nested in the test database's root", otherDatabaseVariable))
	createFixtureTable(c, opened, database, directory)
	nestedTable(c, other)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	direct, err := inspect.Read(ctx, opened.Driver, columnTables(opened), database, nil)

	c.Assert(err, qt.IsNil)
	c.Assert(readBelow(direct, directory), qt.DeepEquals, []string{directory + "|records"})
	c.Assert(readBelow(direct, path.Base(nested)), qt.HasLen, 0)

	client := startProviderFor(c)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id})

	c.Assert(err, qt.IsNil)
	c.Assert(readBelow(snapshot, directory), qt.DeepEquals, []string{directory + "|records"})
	c.Assert(readBelow(snapshot, path.Base(nested)), qt.HasLen, 0)
}
