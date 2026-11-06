//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// planOrderSource declares a group, a table, a topic and a coordination node
// in the directory {dir}, and a grant on each to the group.
const planOrderSource = `roles:
  {group}:
    group: true
tables:
  t:
    schema: {dir}
    columns:
      id: {type: Uint64, primary: true}
topics:
  events:
    schema: {dir}
coordination_nodes:
  workers:
    schema: {dir}
grants:
  grant_t:
    role: {group}
    privileges: [SELECT ROW]
    on_table: {dir}.t
  grant_events:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_topic: {dir}.events
  grant_workers:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_coordination_node: {dir}.workers
`

// `schema apply` and `migrations generate` plan a group, a table, a topic, a
// coordination node and a grant on each in the order the YDB engine built
// into the host at 74de0b65b planned them on 25.1.4.7 and 26.2.1.14: the
// topic, the group, the table, the node, then the grants in the order the
// comparison lists them, the node's, the table's and the topic's. Dropping
// them plans the topic, the table and then the node. The provider planned the
// node ahead of everything, and the grants on the node and the topic ahead of
// the one on the table.
//
// The down file drops the node before the group, where the built-in dropped
// the group first: the reverse plan revokes the group's grant on the node,
// the node's drop carries that revoke out, and a revoke precedes the drop of
// the role it names. The built-in planned no revoke there.
//
// It needs a host whose provider plan keeps the grants on native objects in
// their order among the other grants.
func TestHostPlansCommonObjectsAheadOfANode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	address := os.Getenv("PTAH_YDB_TEST_URL")
	group := "ptahorder" + strings.ToLower(rand.Text())[:8]
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
	})
	source := strings.NewReplacer("{dir}", directory, "{group}", group).Replace(planOrderSource)
	file := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	migrations := c.TempDir()
	scope := []string{"--db-url", address, "--schemas", directory, "--schema-file", file}
	at := func(name string) string { return "`" + directory + "/" + name + "` TO `" + group + "`" }

	planned := hostPlanned(c, directory, "yaml", source)
	generated, generateErr := hostCommand(c, append([]string{"migrations", "generate", "--migrations-dir", migrations, "--name", "order"}, scope...)...)
	up, down := generatedPair(c, migrations)
	applied, applyErr := hostCommand(c, append([]string{"schema", "apply", "--auto-approve", "--tx-mode", "none"}, scope...)...)
	dropped := hostPlanned(c, directory, "yaml", "tables: {}\n")

	c.Assert(planned, qt.DeepEquals, []string{
		"topic-operation " + directory + ".events",
		"CREATE GROUP `" + group + "`",
		"row-table-create-operation " + directory + ".t",
		"coordination-node-operation " + directory + ".workers",
		"GRANT 'ydb.granular.describe_schema' ON " + at("workers"),
		"GRANT 'ydb.granular.select_row' ON " + at("t"),
		"GRANT 'ydb.granular.describe_schema' ON " + at("events"),
	})
	c.Assert(generateErr, qt.IsNil, qt.Commentf("%s", generated))
	c.Assert(renderedStatements(up), qt.DeepEquals, []string{
		"CREATE TOPIC " + directory + "/events", "CREATE GROUP " + group, "CREATE TABLE " + directory + "/t",
		"CREATE COORDINATION NODE " + directory + "/workers",
		"GRANT ON " + directory + "/workers", "GRANT ON " + directory + "/t", "GRANT ON " + directory + "/events",
	}, qt.Commentf("%s", up))
	c.Assert(renderedStatements(down), qt.DeepEquals, []string{
		"DROP TOPIC " + directory + "/events", "DROP TABLE " + directory + "/t",
		"DROP COORDINATION NODE " + directory + "/workers", "DROP GROUP IF EXISTS " + group,
	}, qt.Commentf("%s", down))
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(dropped, qt.DeepEquals, []string{
		"topic-operation " + directory + ".events",
		"table-drop-operation " + directory + ".t",
		"coordination-node-operation " + directory + ".workers",
	})
}
