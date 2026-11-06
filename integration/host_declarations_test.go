//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func TestHostRendersAuthoredSchemaOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	node := "coordination_nodes:\n  workers:\n    schema: app\n    self_check_period: PT2S\n"
	for _, test := range []struct {
		name, filename, source, flag string
		want                         []string
	}{
		{"YAML secret input reference", "desired.yaml", "secrets:\n  z.pw:\n    schema: app\n    value_env: PTAH_SECRET_DECLARATION_INPUT\n", "--schema-file", []string{"CREATE SECRET `app/z.pw` WITH (value = $PTAH_SECRET_DECLARATION_INPUT);"}},
		{"YAML table and native objects", "desired.yaml", hostYAMLTable + node + "topics:\n  events:\n    schema: app\n    consumers:\n      worker: {}\n", "--schema-file", []string{"CREATE TABLE `host_source_items`", "CREATE COORDINATION NODE `app/workers` WITH (self_check_period = Interval('PT2S'))", "CREATE TOPIC `app/events` (CONSUMER `worker`)"}},
		{"Go table and native object", "entities.go", "package entities\n\n" + strings.ReplaceAll(goItems, "{dir}", "app") + "\n//ptah:schema:coordinationnode name=\"workers\" schema=\"app\" self_check_period=\"PT2S\"\ntype Workers struct{}\n", "--root-dir", []string{"CREATE TABLE `app/items`", "CREATE COORDINATION NODE `app/workers` WITH (self_check_period = Interval('PT2S'))"}},
		{"HCL native object", "desired.hcl", "coordination_node \"app\" \"workers\" {\n self_check_period = \"PT2S\"\n}\n", "--schema-file", []string{"CREATE COORDINATION NODE `app/workers` WITH (self_check_period = Interval('PT2S'))"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			directory := c.TempDir()
			file := filepath.Join(directory, test.filename)
			c.Assert(os.WriteFile(file, []byte(test.source), 0o600), qt.IsNil)
			source := file
			if test.flag == "--root-dir" {
				source = directory
			}
			run := func() []byte {
				ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
				defer cancel()
				command := testCommand(ctx, host, "schema", "render", test.flag, source, "--dialect", "ydb", "--server-version", "26.2", "--provider-config", manifest, "--provider-engine", "ydb")
				command.Dir = directory
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %v", stderr.String(), err))
				return output
			}
			output := run()
			for _, want := range test.want {
				c.Assert(string(output), qt.Contains, want)
			}
			c.Assert(string(run()), qt.Equals, string(output))
		})
	}
}

func TestHostOrdersAuthoredNativeCreateBeforeGrantSubprocess(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	file := filepath.Join(c.TempDir(), "desired.yaml")
	source := "roles:\n  readers:\n    group: true\ncoordination_nodes:\n  workers:\n    schema: app\n    self_check_period: PT2S\ngrants:\n  readers_describe:\n    role: readers\n    privileges: [DESCRIBE SCHEMA]\n    on_coordination_node: app.workers\n"
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	command := testCommand(ctx, host, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2", "--provider-config", manifest, "--provider-engine", "ydb")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", stderr.String()))
	text := string(output)
	role := strings.Index(text, "CREATE GROUP `readers`")
	node := strings.Index(text, "CREATE COORDINATION NODE `app/workers`")
	grant := strings.Index(text, "GRANT")
	c.Assert(role >= 0 && node > role && grant > node, qt.IsTrue, qt.Commentf("%s", text))
	c.Assert(text, qt.Contains, "ON `app/workers` TO `readers`")
}

// builtInOrderSource declares a secret and a topic, which the built-in YDB
// engine created first, coordination nodes, which it created after the common
// objects, and grants on a table, on a directory and on two declared objects.
const builtInOrderSource = `roles:
  readers:
    group: true
tables:
  t:
    schema: app
    columns:
      id: {type: Uint64, primary: true}
views:
  v:
    name: app.v
    body: "SELECT id FROM ` + "`app/t`" + `"
topics:
  b_events:
    schema: app
  a_events:
    schema: zed
secrets:
  pw:
    schema: ext
    value_env: PTAH_SECRET_ORDER_INPUT
coordination_nodes:
  workers:
    schema: app
    self_check_period: PT2S
  a_node:
    schema: zed
grants:
  b_grant_table:
    role: readers
    privileges: [SELECT ROW]
    on_table: app.t
  c_grant_dir:
    role: readers
    privileges: [LIST]
    on_schema: app
  d_grant_topic:
    role: readers
    privileges: [DESCRIBE SCHEMA]
    on_topic: zed.a_events
  z_grant_node:
    role: readers
    privileges: [DESCRIBE SCHEMA]
    on_coordination_node: app.workers
`

// renderedStatements names each statement of a schema render by the words in
// front of the first object it names and that object, such as `CREATE TOPIC
// app/events`, and a grant by the object it is on, such as `GRANT ON app/t`.
func renderedStatements(output string) []string {
	var named []string
	for text := range strings.SplitSeq(output, ";\n") {
		var lines []string
		for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
			if line != "" && !strings.HasPrefix(line, "--") {
				lines = append(lines, line)
			}
		}
		statement := strings.Join(lines, " ")
		if statement == "" {
			continue
		}
		if _, on, found := strings.Cut(statement, " ON `"); found && strings.HasPrefix(statement, "GRANT ") {
			object, _, _ := strings.Cut(on, "`")
			named = append(named, "GRANT ON "+object)
			continue
		}
		words, rest, _ := strings.Cut(statement, " `")
		object, _, _ := strings.Cut(rest, "`")
		named = append(named, words+" "+object)
	}
	return named
}

// A schema render writes the authored objects where the built-in YDB engine
// at host 74de0b65b wrote them (`schema render --dialect ydb --server-version
// 26.2`): the secret and the topics first, then the roles, the tables, the
// views and the grants on them, then the coordination nodes, and last the
// grants on the objects the document declares. The provider wrote the nodes ahead of the roles, or between the
// table and the view.
func TestHostRendersAuthoredObjectsInTheBuiltInOrderSubprocess(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	file := filepath.Join(c.TempDir(), "desired.yaml")
	c.Assert(os.WriteFile(file, []byte(builtInOrderSource), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	command := testCommand(ctx, host, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2", "--provider-config", manifest, "--provider-engine", "ydb")
	var stderr bytes.Buffer
	command.Stderr = &stderr

	output, err := command.Output()

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stderr.String()))
	c.Assert(renderedStatements(string(output)), qt.DeepEquals, []string{
		"CREATE SECRET ext/pw", "CREATE TOPIC app/b_events", "CREATE TOPIC zed/a_events",
		"CREATE GROUP readers", "CREATE TABLE app/t", "CREATE VIEW app/v", "GRANT ON app/t", "GRANT ON app",
		"CREATE COORDINATION NODE app/workers", "CREATE COORDINATION NODE zed/a_node",
		"GRANT ON zed/a_events", "GRANT ON app/workers",
	}, qt.Commentf("%s", output))
}
