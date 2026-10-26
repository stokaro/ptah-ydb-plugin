//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

func TestHostRendersAuthoredSchemaOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
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
			c := qt.New(t)
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
				command := exec.CommandContext(ctx, host, "schema", "render", test.flag, source, "--dialect", "ydb", "--server-version", "26.2", "--provider-config", manifest, "--provider-engine", "ydb")
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
	c := qt.New(t)
	host, manifest := hostManifest(c)
	file := filepath.Join(c.TempDir(), "desired.yaml")
	source := "roles:\n  readers:\n    group: true\ncoordination_nodes:\n  workers:\n    schema: app\n    self_check_period: PT2S\ngrants:\n  readers_describe:\n    role: readers\n    privileges: [DESCRIBE SCHEMA]\n    on_coordination_node: app.workers\n"
	c.Assert(os.WriteFile(file, []byte(source), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, host, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2", "--provider-config", manifest, "--provider-engine", "ydb")
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
