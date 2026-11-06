//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// replicationOrderSource declares a group, a table src an async replication
// mirror copies, a table t, a topic events and a grant to the group on the
// replication, on t and on events, in the directory {dir}. {source} is the
// absolute path of src and {connection} the server's own connection string.
const replicationOrderSource = `roles:
  {group}:
    group: true
tables:
  t:
    schema: {dir}
    columns:
      id: {type: Uint64, primary: true}
  src:
    schema: {dir}
    columns:
      id: {type: Int64, primary: true, not_null: true}
      note: {type: Utf8}
topics:
  events:
    schema: {dir}
async_replications:
  mirror:
    schema: {dir}
    connection_string: {connection}
    items:
      - source: {source}
        target: {dir}/rep
grants:
  grant_t:
    role: {group}
    privileges: [SELECT ROW]
    on_table: {dir}.t
  grant_events:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_topic: {dir}.events
  grant_mirror:
    role: {group}
    privileges: [ydb.generic.read]
    on_async_replication: {dir}.mirror
`

// `schema apply` and `migrations generate` create an async replication after
// every table and before the grants, as the YDB engine built into the host at
// 74de0b65b did on 25.1.4.7 and 26.2.1.14: the topic, the group, the two
// tables, the replication, then the grants on the replication, on t and on
// events. The down file drops the replication first, as the built-in's did.
// The provider created the replication right after the table it copies,
// ahead of the other table, since the host breaks a tie by owner and this
// provider's owner sorts first.
func TestHostCreatesAReplicationAfterTheTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	group := principalName("ptahrepo")
	source := strings.NewReplacer("{dir}", directory, "{group}", group, "{source}", path.Join(database, directory, "src"),
		"{connection}", strconv.Quote(selfConnection(c, opened))).Replace(replicationOrderSource)
	file := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	migrations := c.TempDir()
	at := func(name string) string { return "`" + directory + "/" + name + "` TO `" + group + "`" }

	planned := hostPlanned(c, directory, "yaml", source)
	generated, generateErr := hostCommand(c, "migrations", "generate", "--migrations-dir", migrations, "--name", "order",
		"--db-url", address, "--schemas", directory, "--schema-file", file)
	up, down := generatedPair(c, migrations)

	c.Assert(planned, qt.DeepEquals, []string{
		"topic-operation " + directory + ".events",
		"CREATE GROUP `" + group + "`",
		"row-table-create-operation " + directory + ".src",
		"row-table-create-operation " + directory + ".t",
		"async-replication-operation " + directory + ".mirror",
		"GRANT 'ydb.generic.read' ON " + at("mirror"),
		"GRANT 'ydb.granular.select_row' ON " + at("t"),
		"GRANT 'ydb.granular.describe_schema' ON " + at("events"),
	})
	c.Assert(generateErr, qt.IsNil, qt.Commentf("%s", generated))
	c.Assert(renderedStatements(up), qt.DeepEquals, []string{
		"CREATE TOPIC " + directory + "/events", "CREATE GROUP " + group, "CREATE TABLE " + directory + "/src", "CREATE TABLE " + directory + "/t",
		"CREATE ASYNC REPLICATION " + directory + "/mirror",
		"GRANT ON " + directory + "/mirror", "GRANT ON " + directory + "/t", "GRANT ON " + directory + "/events",
	}, qt.Commentf("%s", up))
	c.Assert(renderedStatements(down), qt.DeepEquals, []string{
		"DROP ASYNC REPLICATION " + directory + "/mirror", "DROP TOPIC " + directory + "/events",
		"DROP TABLE " + directory + "/src", "DROP TABLE " + directory + "/t", "DROP GROUP IF EXISTS " + group,
	}, qt.Commentf("%s", down))
}
