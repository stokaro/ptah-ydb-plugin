//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// streamingOrderSource declares a group, a table t, the topics events, input
// and output, a streaming query copy from input to output, and a grant to the
// group on t, on events and on the query, in the directory {dir}.
const streamingOrderSource = `roles:
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
  input:
    schema: {dir}
  output:
    schema: {dir}
streaming_queries:
  copy:
    schema: {dir}
    text: "INSERT INTO ` + "`{dir}/output`" + ` SELECT * FROM ` + "`{dir}/input`" + `;"
grants:
  grant_t:
    role: {group}
    privileges: [SELECT ROW]
    on_table: {dir}.t
  grant_events:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_topic: {dir}.events
  grant_query:
    role: {group}
    privileges: [DESCRIBE SCHEMA]
    on_streaming_query: {dir}.copy
`

// `schema apply` and `migrations generate` create a streaming query after the
// grants on the other objects and grant on it last, as the YDB engine built
// into the host at 74de0b65b did on 26.2.1.14: the topics, the group, the
// table, the grants on t and on events, the query and the grant on it. The
// down file drops the query first, as the built-in's did, and the rollback of
// the drop creates the query after the grants again. The provider created the
// query right after the topics, ahead of the group, the table and the grants,
// and granted on it first.
func TestHostCreatesAStreamingQueryAfterTheGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _, _ := streamingFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	group := principalName("ptahstream")
	source := strings.NewReplacer("{dir}", directory, "{group}", group).Replace(streamingOrderSource)
	root := c.TempDir()
	file, emptied := filepath.Join(root, "schema.yaml"), filepath.Join(root, "emptied.yaml")
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(emptied, []byte("roles:\n  "+group+":\n    group: true\ntables: {}\n"), 0o600), qt.IsNil)
	created, dropped := c.TempDir(), c.TempDir()
	at := func(object string) string { return "`" + directory + "/" + object + "` TO `" + group + "`" }
	object := func(verb, name string) string { return verb + " " + directory + "/" + name }

	planned := hostPlanned(c, directory, "yaml", source)
	generated, generateErr := hostCommand(c, "migrations", "generate", "--migrations-dir", created, "--name", "order",
		"--db-url", address, "--schemas", directory, "--schema-file", file)
	up, down := generatedPair(c, created)
	applied, applyErr := hostApplySource(c, directory, "yaml", source)
	dropGenerated, dropErr := hostCommand(c, "migrations", "generate", "--migrations-dir", dropped, "--name", "drop",
		"--db-url", address, "--schemas", directory, "--schema-file", emptied)
	dropUp, dropDown := generatedPair(c, dropped)

	c.Assert(planned, qt.DeepEquals, []string{
		"topic-operation " + directory + ".events", "topic-operation " + directory + ".input", "topic-operation " + directory + ".output",
		"CREATE GROUP `" + group + "`",
		"row-table-create-operation " + directory + ".t",
		"GRANT 'ydb.granular.select_row' ON " + at("t"),
		"GRANT 'ydb.granular.describe_schema' ON " + at("events"),
		"streaming-query-operation " + directory + ".copy",
		"GRANT 'ydb.granular.describe_schema' ON " + at("copy"),
	})
	c.Assert(generateErr, qt.IsNil, qt.Commentf("%s", generated))
	creation := []string{
		object("CREATE TOPIC", "events"), object("CREATE TOPIC", "input"), object("CREATE TOPIC", "output"), "CREATE GROUP " + group,
		object("CREATE TABLE", "t"), object("GRANT ON", "t"), object("GRANT ON", "events"), object("CREATE STREAMING QUERY", "copy"), "END DO ",
		object("GRANT ON", "copy"),
	}
	c.Assert(renderedStatements(up), qt.DeepEquals, creation, qt.Commentf("%s", up))
	c.Assert(renderedStatements(down), qt.DeepEquals, []string{
		object("DROP STREAMING QUERY", "copy"), object("DROP TOPIC", "events"), object("DROP TOPIC", "input"), object("DROP TOPIC", "output"),
		object("DROP TABLE", "t"), "DROP GROUP IF EXISTS " + group,
	}, qt.Commentf("%s", down))
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(dropErr, qt.IsNil, qt.Commentf("%s", dropGenerated))
	c.Assert(renderedStatements(dropUp), qt.DeepEquals, []string{
		object("DROP STREAMING QUERY", "copy"), object("DROP TOPIC", "events"), object("DROP TOPIC", "input"), object("DROP TOPIC", "output"),
		object("DROP TABLE", "t"),
	}, qt.Commentf("%s", dropUp))
	c.Assert(renderedStatements(dropDown), qt.DeepEquals, slices.Delete(slices.Clone(creation), 3, 4), qt.Commentf("%s", dropDown))
}
