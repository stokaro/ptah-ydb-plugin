//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A rehearsal on the target's own URL recreates the target's current schema
// in a dev realm of the database. This provider binds every value it reads to
// the database it read, so the host moves the target's values into the realm
// before the realm compares them; without the move the provider refused the
// comparison (`YDB comparison has conflicting database contexts`, which the
// host printed as `provider compare models failed (InvalidArgument)`). The
// built-in YDB engine at host 74de0b65b rehearsed and applied every one of
// these on 25.1.4.7 and 26.2.1.14.

// rehearsalWithPools turns resource pools on for the test and checks that the
// target holds the pool default. Every database of a server with resource
// pools holds it, a read of a directory reports it to a host that lists
// schema.database-kinds, and a dev realm reads no pool: they are its
// database's. So every rehearsal on such a server compares a pool the realm
// does not read.
func rehearsalWithPools(c *qt.C) (opened *connection.Connection, directory, database string) {
	c.Helper()
	host, manifest := hostManifest(c)
	c.Assert(hostNegotiates(c, host, manifest, provider.DatabaseKindsFeature), qt.IsTrue,
		qt.Commentf("a read of a directory reports the database's pools only to a host that lists %s", provider.DatabaseKindsFeature))
	opened, directory, database = rehearsalFixture(c)
	enableResourcePools(c, opened)
	pools, _ := readHeldWorkload(c, c.Context(), opened.DB, database, []string{"default"}, nil)
	c.Assert(pools, qt.HasLen, 1, qt.Commentf("the database holds the pool default"))
	return opened, directory, database
}

// `schema apply --dev-url` with resource pools on rehearses a new table in the
// realm and creates it in the target.
func TestHostRehearsesWithResourcePoolsOn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalWithPools(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	applied, err := rehearsedApply(c, entities, directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE})
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}

// `ptah-compat schema apply --dev-url` with resource pools on rehearses the
// same way, from an SQL source.
func TestHostCompatRehearsesWithResourcePoolsOn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalWithPools(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	desired := "file://" + compatFile(c, c.TempDir(), "items.sql", "CREATE TABLE `"+directory+"/items` (id Int64 NOT NULL, title Utf8, PRIMARY KEY (id));\n")

	applied, notes, exit := runCompat(c, nil, "schema", "apply", "--url", address, "--to", desired, "--schema", directory, "--dev-url", address, "--auto-approve")

	c.Assert(exit, qt.Equals, 0, qt.Commentf("%s\n%s", applied, notes))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items")), qt.DeepEquals, map[string]string{"id": "Int64", "title": "Optional<Utf8>"})
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}

// rehearsalObjects is a YAML source with a table, a topic and a coordination
// node in directory, and columns the lines of the table's columns after its
// key.
func rehearsalObjects(directory, columns string) string {
	return "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, nullable: false}\n" + columns +
		"topics:\n  events:\n    schema: " + directory + "\n" +
		"coordination_nodes:\n  workers:\n    schema: " + directory + "\n"
}

// A change to a directory that holds a topic and a coordination node is
// rehearsed in a realm that holds them too: the host moves the target's topic
// and node into the realm and creates them there, the change planned for the
// realm is the target's, and the target gets the column. This holds on a
// server without resource pools as well.
func TestHostRehearsalRecreatesTheTargetsTopicAndNode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := rehearsalFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	created, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory, "--auto-approve",
		"--schema-file", writeFiles(c, c.TempDir(), map[string]string{"s.yaml": rehearsalObjects(directory, "")})+"/s.yaml")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	changed := writeFiles(c, c.TempDir(), map[string]string{"s.yaml": rehearsalObjects(directory, "      title: {type: Utf8}\n")}) + "/s.yaml"

	applied, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", directory, "--schema-file", changed, "--dev-url", address, "--auto-approve")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{
		"items": Ydb_Scheme.Entry_TABLE, "events": Ydb_Scheme.Entry_TOPIC, "workers": Ydb_Scheme.Entry_COORDINATION_NODE,
	})
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items")), qt.DeepEquals, map[string]string{"id": "Uint64", "title": "Optional<Utf8>"})
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}
