//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests in this file run the host's reading surfaces, which have no YDB
// code of their own, over this provider's read.

// execAll runs schema statements, one per query, with the fixture's
// connection.
func execAll(c *qt.C, f migrationsFixture, statements ...string) {
	c.Helper()
	for _, statement := range statements {
		c.Assert(execFixtureSchema(c.Context(), f.opened.DB, statement), qt.IsNil, qt.Commentf("execute: %s", statement))
	}
}

// `schema stats` counts the tables, columns and indexes of a directory that
// also holds a topic, a changefeed, their consumers and a coordination node.
// The labels name the dialect and the directory the counts came from.
func TestHostSchemaStatsCountsWhatTheReadDescribes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	absolute := func(name string) string { return path.Join(f.database, f.dir, name) }
	execAll(c, f,
		"CREATE TABLE `"+absolute("a")+"` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id), INDEX a_name GLOBAL SYNC ON (name))",
		"CREATE TABLE `"+absolute("b")+"` (x Uint64 NOT NULL, y Utf8 NOT NULL, z Bool, PRIMARY KEY (x, y))",
		"CREATE TOPIC `"+absolute("events")+"` (CONSUMER `one`, CONSUMER `two`)",
		"ALTER TABLE `"+absolute("a")+"` ADD CHANGEFEED `changes` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"ALTER TOPIC `"+absolute("a/changes")+"` ADD CONSUMER `reader`",
	)
	c.Assert(f.opened.Driver.Coordination().CreateNode(c.Context(), absolute("locks"), coordination.NodeConfig{}), qt.IsNil)

	stdout, err := hostCommand(c, "schema", "stats", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	labels := `{dialect="ydb",schemas="` + f.dir + `"}`
	for _, sample := range []string{
		"ptah_schema_tables" + labels + " 2\n",
		"ptah_schema_columns" + labels + " 5\n",
		"ptah_schema_indexes" + labels + " 1\n",
		"ptah_schema_constraints" + labels + " 0\n",
		"ptah_schema_views" + labels + " 0\n",
	} {
		c.Assert(stdout, qt.Contains, sample)
	}
	c.Assert(stdout, qt.Matches, `(?s).*# EOF\n`)
}

// The host's TestYDBSchemaStats_CountsWhatTheReaderDescribes. For a host that
// negotiates models.report-counts, the provider counts the topic, the
// changefeed, their consumers and the coordination node of the directory the
// test above counts, under the lines of the built-in YDB engine, help text
// included. The directory states no table setting, and each table setting
// family has its line at zero, as the built-in engine printed it.
func TestHostSchemaStatsCountsTheNativeObjects_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ReportCountsFeature)
	f := newMigrationsFixture(c)
	absolute := func(name string) string { return path.Join(f.database, f.dir, name) }
	execAll(c, f,
		"CREATE TABLE `"+absolute("a")+"` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id), INDEX a_name GLOBAL SYNC ON (name))",
		"CREATE TABLE `"+absolute("b")+"` (x Uint64 NOT NULL, y Utf8 NOT NULL, z Bool, PRIMARY KEY (x, y))",
		"CREATE TOPIC `"+absolute("events")+"` (CONSUMER `one`, CONSUMER `two`)",
		"ALTER TABLE `"+absolute("a")+"` ADD CHANGEFEED `changes` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"ALTER TOPIC `"+absolute("a/changes")+"` ADD CONSUMER `reader`",
	)
	c.Assert(f.opened.Driver.Coordination().CreateNode(c.Context(), absolute("locks"), coordination.NodeConfig{}), qt.IsNil)

	stdout, err := hostCommand(c, "schema", "stats", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	labels := `{dialect="ydb",schemas="` + f.dir + `"}`
	for _, sample := range []string{
		"ptah_schema_tables" + labels + " 2\n",
		"ptah_schema_topics" + labels + " 1\n",
		"ptah_schema_topic_consumers" + labels + " 2\n",
		"ptah_schema_changefeeds" + labels + " 1\n",
		"ptah_schema_changefeed_consumers" + labels + " 1\n",
		"ptah_schema_coordination_nodes" + labels + " 1\n",
		"ptah_schema_streaming_queries" + labels + " 0\n",
		"ptah_schema_ydb_ttl_tables" + labels + " 0\n",
		"ptah_schema_ydb_column_families" + labels + " 0\n",
		"ptah_schema_ydb_table_partitioning" + labels + " 0\n",
		"ptah_schema_ydb_index_partitioning" + labels + " 0\n",
		"ptah_schema_ydb_column_tables" + labels + " 0\n",
		"ptah_schema_ydb_tiered_ttl_tables" + labels + " 0\n",
		"ptah_schema_ydb_vector_indexes" + labels + " 0\n",
		"# HELP ptah_schema_topics Standalone topics.\n",
		"# HELP ptah_schema_topic_consumers Consumers of standalone topics.\n",
		"# HELP ptah_schema_changefeeds Table changefeeds.\n",
		"# HELP ptah_schema_changefeed_consumers Consumers of table changefeeds.\n",
		"# HELP ptah_schema_coordination_nodes Standalone coordination nodes.\n",
		"# HELP ptah_schema_streaming_queries Standalone streaming queries.\n",
	} {
		c.Assert(stdout, qt.Contains, sample)
	}
}

// `schema security` over a directory holding a table granted to a group with
// no member reports the group as granting to nobody, and lists the row-level
// security rule, which YDB gives nothing to check, as not checked. The
// directory is owned by root@builtin, which .sys/auth_users does not list, so
// OWN01 names it as not checked rather than refusing the report.
func TestHostSchemaSecurityReadsTheAccessModel_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	group := principalName("ptahsecgroup")
	c.Cleanup(func() { _ = execFixtureSchema(context.Background(), f.opened.DB, "DROP GROUP IF EXISTS "+group) })
	execAll(c, f,
		"CREATE TABLE `"+path.Join(f.database, f.dir, "orders")+"` (id Int64 NOT NULL, PRIMARY KEY (id))",
		"CREATE GROUP "+group,
		"GRANT 'ydb.generic.read' ON `"+path.Join(f.database, f.dir, "orders")+"` TO "+group,
	)

	stdout, err := hostCommand(c, "schema", "security", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir, "--format", "json")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	c.Assert(stdout, qt.Contains, `"code": "ROL04"`)
	c.Assert(stdout, qt.Contains, `"name": "`+group+`"`)
	c.Assert(stdout, qt.Matches, `(?s).*"code": "PRV01",\s*"reason": "the target does not model row-level security".*`)
	c.Assert(stdout, qt.Matches, `(?s).*"code": "OWN01",\s*"reason": "the read did not establish whether [^"]*root@builtin[^"]* can log in, so the objects they own were not checked".*`)
}

// A user who may list the database and may not read its rows, a member of
// METADATA-READERS alone, cannot read the users and groups YDB reports only
// through .sys/auth_*, so `schema security` refuses rather than report a clean
// access model it never saw. The built-in engine's refusal says what the
// connection needs (`the analysis reads the database's roles, and this
// connection may not read them, ...`); through a provider the host says which
// part of the read is missing.
func TestHostSchemaSecurityRefusesUnreadPrincipals_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	user := principalName("ptahseclister")
	const password = "Lister1!"
	c.Cleanup(func() { _ = execFixtureSchema(context.Background(), f.opened.DB, "DROP USER IF EXISTS "+user) })
	execAll(c, f, "CREATE USER "+user+" PASSWORD '"+password+"'", "ALTER GROUP `METADATA-READERS` ADD USER "+user)
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	parsed.User = url.UserPassword(user, password)

	stdout, err := hostCommand(c, "schema", "security", "--db-url", parsed.String())

	c.Assert(err, qt.IsNotNil)
	c.Assert(stdout, qt.Equals, "error: the read did not inspect role_membership; a security report needs complete access metadata\n")
}

// A lineage traces views and routines. YDB has no routines, so a directory
// holding only tables has nothing to trace, and the command says so.
func TestHostSchemaLineageOfTablesOnly_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	execAll(c, f, "CREATE TABLE `"+path.Join(f.database, f.dir, "t")+"` (id Int64 NOT NULL, PRIMARY KEY (id))")

	stdout, err := hostCommand(c, "schema", "lineage", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	c.Assert(stdout, qt.Contains, "No view or routine columns to trace.\n")
}

// A lineage of a YDB directory traces its views from the queries the server
// stores: a view's source is its table, a view over a view links to the view
// it reads, a star resolves to the table's columns, and a double-quoted YQL
// string feeds no column.
func TestHostSchemaLineageTracesViews_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	absolute := func(name string) string { return path.Join(f.database, f.dir, name) }
	execAll(c, f,
		"CREATE TABLE `"+absolute("items")+"` (id Int64 NOT NULL, label Utf8, PRIMARY KEY (id))",
		"CREATE VIEW `"+absolute("named")+"` WITH (security_invoker = TRUE) AS SELECT id, label AS name, \"fixed\" AS tag FROM `"+f.table("items")+"`",
		"CREATE VIEW `"+absolute("names")+"` WITH (security_invoker = TRUE) AS SELECT name FROM `"+f.table("named")+"`",
		"CREATE VIEW `"+absolute("everything")+"` WITH (security_invoker = TRUE) AS SELECT * FROM `"+f.table("items")+"`",
	)
	c.Cleanup(func() {
		for _, view := range []string{"names", "named", "everything"} {
			_ = execFixtureSchema(context.Background(), f.opened.DB, "DROP VIEW `"+absolute(view)+"`")
		}
	})

	stdout, err := hostCommand(c, "schema", "lineage", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	d := f.dir
	c.Assert(strings.Fields(stdout), qt.DeepEquals, strings.Fields(""+
		"SOURCE FEEDS KIND\n"+
		d+".items.id "+d+".everything.id view\n"+
		d+".items.label "+d+".everything.label view\n"+
		d+".items.id "+d+".named.id view\n"+
		d+".items.label "+d+".named.name view\n"+
		d+".named.name "+d+".names.name view\n"))
}

// mcpCall runs `ptah mcp` against the test database, as an AI client starts
// it, sends the protocol's initialization and one tools/call, and returns the
// reply to that call.
func mcpCall(c *qt.C, tool string, arguments map[string]any) string {
	c.Helper()
	return mcpCallOn(c, os.Getenv("PTAH_YDB_TEST_URL"), nil, tool, arguments)
}

// mcpCallOn is mcpCall against the database at address, with the further
// flags of `ptah mcp` given, such as the roots a specification may be read
// from.
func mcpCallOn(c *qt.C, address string, flags []string, tool string, arguments map[string]any) string {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, append(append([]string{"mcp", "--database-url", address, "--database-class", "ephemeral",
		"--allow-database-inspect", "allow"}, flags...), "--provider-config", manifest, "--provider-engine", "ydb")...)
	stdin, err := cmd.StdinPipe()
	c.Assert(err, qt.IsNil)
	stdout, err := cmd.StdoutPipe()
	c.Assert(err, qt.IsNil)
	c.Assert(cmd.Start(), qt.IsNil)
	call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	c.Assert(err, qt.IsNil)
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"ptah-ydb-plugin","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		string(call),
	} {
		_, err := stdin.Write([]byte(message + "\n"))
		c.Assert(err, qt.IsNil)
	}
	reply := ""
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	for lines.Scan() {
		if strings.Contains(lines.Text(), `"id":2`) {
			reply = lines.Text()
			break
		}
	}
	c.Assert(stdin.Close(), qt.IsNil)
	c.Assert(cmd.Wait(), qt.IsNil)
	return reply
}

// The agent surface's read_database takes the YDB target the operator
// configured, and reads the row tables of the directories the caller names.
func TestHostAgentReadsTheNamedDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	execAll(c, f, "CREATE TABLE `"+path.Join(f.database, f.dir, "notes")+"` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id))")
	var version string
	c.Assert(f.opened.DB.QueryRowContext(c.Context(), "SELECT Version()").Scan(&version), qt.IsNil)

	reply := mcpCall(c, "read_database", map[string]any{"schemas": []string{f.dir}})

	var response struct {
		Result struct {
			StructuredContent struct {
				Dialect string `json:"dialect"`
				Version string `json:"version"`
				Objects []struct {
					Kind, Schema, Name string
					Columns            int
				} `json:"objects"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	c.Assert(json.Unmarshal([]byte(reply), &response), qt.IsNil, qt.Commentf("%s", reply))
	read := response.Result.StructuredContent
	c.Assert(read.Dialect, qt.Equals, "ydb")
	c.Assert(read.Version, qt.Equals, version)
	c.Assert(read.Objects, qt.DeepEquals, []struct {
		Kind, Schema, Name string
		Columns            int
	}{{Kind: "table", Schema: f.dir, Name: "notes", Columns: 2}})
}

// A key column YDB reports as nullable has no declaration, so `introspect`
// refuses it and writes nothing, rather than write a key Ptah would rebuild
// NOT NULL.
func TestHostIntrospectRefusesANullableKeyColumn_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	execAll(c, f, "CREATE TABLE `"+path.Join(f.database, f.dir, "keys")+"` (k Utf8, v Int64, PRIMARY KEY (k))")
	out := filepath.Join(c.TempDir(), "models")

	stdout, err := hostCommand(c, "introspect", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir, "--out", out)

	c.Assert(err, qt.IsNotNil)
	c.Assert(stdout, qt.Contains, `column "k" of table "`+f.dir+`/keys" is a nullable key column, `+
		`which no Ptah declaration can represent: Ptah writes NOT NULL on every YDB key column`)
	_, statErr := os.Stat(out)
	c.Assert(statErr, qt.ErrorIs, os.ErrNotExist)
}
