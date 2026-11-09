//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// nestedTable creates the table items in a directory of its own in the
// database other, which a read that descended into it would report, and
// returns the directory.
func nestedTable(c *qt.C, other *connection.Connection) string {
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
	return directory
}

// readBelow lists the tables and native objects of snapshot whose directory is
// directory or lies below it, as directory|name.
func readBelow(snapshot *protocol.Schema, directory string) []string {
	var read []string
	below := func(held string) bool { return held == directory || strings.HasPrefix(held, directory+"/") }
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
	_ = nestedTable(c, other)
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

// A selector that names a nested database, or a directory it holds, reads as
// an empty directory, directly and through the provider subprocess with the
// common objects, their access and subtree coverage, as the host's embedded
// engine at 74de0b65b read it on 25.1.4.7 and 26.2.1.14. Read as a directory to
// list, the selection is refused (`YDB schema selector did not describe the
// expected directory` at the root, `its parent is not a directory` below it),
// and so is every read, introspection and apply that names it.
func TestSelectedReadOfANestedDatabaseSubprocess(t *testing.T) {
	c := testcheck.New(t)
	_, other := otherDatabase(c)
	opened, _, database := inspectionFixture(c)
	c.Assert(path.Dir(other.Driver.Name()), qt.Equals, database, qt.Commentf("%s names a database nested in the test database's root", otherDatabaseVariable))
	nested := path.Base(other.Driver.Name())
	directory := nestedTable(c, other)
	client := startProviderFor(c, provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.SchemaSubtreeCoverageFeature)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	for _, test := range []struct{ name, selector string }{
		{name: "the nested database's root", selector: nested},
		{name: "a directory the nested database holds", selector: nested + "/" + directory},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			direct, directErr := inspect.Read(ctx, opened.Driver, columnTables(opened), database, []string{test.selector})
			snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{test.selector}})

			c.Assert(directErr, qt.IsNil)
			c.Assert(readBelow(direct, nested), qt.HasLen, 0)
			c.Assert(err, qt.IsNil)
			c.Assert(readBelow(snapshot, nested), qt.HasLen, 0)
		})
	}
}

// The host reads a selection of a nested database as an empty directory, as
// its embedded engine did, and refuses a table, a topic or a coordination
// node declared there, in words that name the nested database. The read holds
// nothing there, so without the refusal the plan creates the object: the
// embedded engine planned `CREATE TABLE`, which YDB ran through the session
// of the test database into the nested one, and every later plan created it
// again and failed (`path exist`).
func TestHostRefusesACreationInANestedDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, other := otherDatabase(c)
	_, _, database := lockFixture(c)
	c.Assert(path.Dir(other.Driver.Name()), qt.Equals, database, qt.Commentf("%s names a database nested in the test database's root", otherDatabaseVariable))
	nested := path.Base(other.Driver.Name())
	directory := nestedTable(c, other)
	address := os.Getenv("PTAH_YDB_TEST_URL")

	read, readErr := hostCommand(c, "db", "read", "--db-url", address, "--schemas", nested+","+nested+"/"+directory)

	c.Assert(readErr, qt.IsNil, qt.Commentf("%s", read))
	c.Assert(read, qt.Not(qt.Contains), "items")
	for _, test := range []struct{ name, selector, source, want string }{
		{name: "a table at the root", selector: nested,
			source: "tables:\n  g166_items:\n    schema: " + nested + "\n    columns:\n      id: {type: Uint64, primary: true, nullable: false}\n",
			want:   "this plan creates the table " + nested + "/g166_items inside " + other.Driver.Name()},
		{name: "a table in a directory it holds", selector: nested + "/" + directory,
			source: "tables:\n  g166_items:\n    schema: " + nested + "/" + directory + "\n    columns:\n      id: {type: Uint64, primary: true, nullable: false}\n",
			want:   "this plan creates the table " + nested + "/" + directory + "/g166_items inside " + other.Driver.Name()},
		{name: "a topic", selector: nested, source: "topics:\n  g166_events:\n    schema: " + nested + "\n",
			want: "this plan creates the topic " + nested + "/g166_events inside " + other.Driver.Name()},
		{name: "a coordination node", selector: nested, source: "coordination_nodes:\n  g166_workers:\n    schema: " + nested + "\n",
			want: "this plan creates the coordination node " + nested + "/g166_workers inside " + other.Driver.Name()},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source := writeFiles(c, c.TempDir(), map[string]string{"s.yaml": test.source}) + "/s.yaml"

			output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", test.selector, "--schema-file", source, "--dry-run")

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.want+", which is another YDB database nested in "+database)
		})
	}
}

// nestedClaims lists the distinct claims snapshot makes for a nested
// database, written as the claimed directory and whether the claim covers the
// directories below it.
func nestedClaims(snapshot *protocol.Schema) []string {
	var claimed []string
	for _, record := range snapshot.GetCoverage().GetKinds() {
		for _, region := range record.GetRegions() {
			root, nested := schema.NestedDatabase(region.GetKnowledge())
			claimed = append(claimed, fmt.Sprintf("%s subtree=%t root=%s nested=%t", region.GetScope().GetSchema().GetNormalized(), region.GetScope().GetSchemaSubtree(), root, nested))
		}
	}
	claimed = slices.DeleteFunc(claimed, func(claim string) bool { return strings.HasSuffix(claim, "nested=false") })
	slices.Sort(claimed)
	return slices.Compact(claimed)
}

// A read of the whole test database claims the nested database's root with
// its reason, with subtree where the host takes subtree regions, so that the
// comparison and the planning check refuse a creation there: the read holds
// nothing there, and a plan that reads that as absence creates what a
// declaration places there, which YDB runs into the nested database.
func TestRootReadClaimsANestedDatabaseSubprocess(t *testing.T) {
	c := testcheck.New(t)
	_, other := otherDatabase(c)
	opened, _, database := inspectionFixture(c)
	nested := path.Base(other.Driver.Name())
	c.Assert(path.Dir(other.Driver.Name()), qt.Equals, database, qt.Commentf("%s names a database nested in the test database's root", otherDatabaseVariable))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := startProviderFor(c, provider.SchemaSubtreeCoverageFeature)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	direct, directErr := inspect.Read(ctx, opened.Driver, columnTables(opened), database, nil)
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id})

	c.Assert(directErr, qt.IsNil)
	c.Assert(nestedClaims(direct), qt.DeepEquals, []string{nested + " subtree=false root=" + nested + " nested=true"})
	c.Assert(err, qt.IsNil)
	c.Assert(nestedClaims(snapshot), qt.DeepEquals, []string{nested + " subtree=true root=" + nested + " nested=true"})
}
