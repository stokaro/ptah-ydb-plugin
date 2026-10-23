//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// hostSource runs the pinned host's `schema plan --dry-run` over one source
// file with this provider selected for ydb, and returns what the host reported.
// The host reads the database before it plans, so the live address is needed;
// the plan itself is not the subject. What the test establishes is how far the
// host's frontend gets with a table the source declares: whether it sends the
// provider the claimed keys and directives, and what the provider answers.
// This is an external binary contract test; the provider module imports no
// host package.
func hostSource(c *qt.C, flag, name, content string) string {
	c.Helper()
	host := os.Getenv("PTAH_HOST_TEST_BIN")
	if host == "" {
		c.Skip("PTAH_HOST_TEST_BIN is required for the external host contract")
	}
	binary, digest := providerBinary(c)
	configuration := map[string]any{"format_version": 2, "providers": []any{map[string]any{
		"executable": binary, "sha256": digest, "provider_id": schema.ProviderID, "engines": []any{map[string]any{"id": "ydb", "aliases": []string{"ydbs"}}}, "environment": []string{},
	}}}
	encoded, err := json.Marshal(configuration)
	c.Assert(err, qt.IsNil)
	directory := c.TempDir()
	manifest := filepath.Join(directory, "providers.json")
	c.Assert(os.WriteFile(manifest, encoded, 0o600), qt.IsNil)
	source := filepath.Join(directory, "source", name)
	c.Assert(os.MkdirAll(filepath.Dir(source), 0o700), qt.IsNil)
	c.Assert(os.WriteFile(source, []byte(content), 0o600), qt.IsNil)
	target := source
	if flag == "--root-dir" {
		target = filepath.Dir(source)
	}
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "schema", "plan", "--dry-run", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), flag, target)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	_ = cmd.Run() // The plan may fail for reasons that are not the subject.
	return output.String()
}

const (
	hostYAMLTable = "tables:\n  host_source_items:\n    auto_partitioning_by_load: ENABLED\n    auto_partitioning_min_partitions_count: 2\n    read_replicas_settings: ANY_AZ:1\n" +
		"    column_families:\n      default: {compression: lz4}\n    columns:\n      id: {type: Uint64, primary: true}\n      body: {type: Utf8}\n"
	hostGoTable = "package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"default\" compression=\"lz4\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n"
)

// The host hands the provider a table's partitioning and column families, and
// the provider accepts them: whatever the host reports next, it is not a
// refusal of the declaration.
func TestHostSendsTableDeclarationsToTheProvider_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, flag, file, content string }{
		{"YAML keys", "--schema-file", "schema.yaml", hostYAMLTable},
		{"a Go directive", "--root-dir", "entities.go", hostGoTable},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			output := hostSource(c, test.flag, test.file, test.content)
			c.Assert(output, qt.Not(qt.Matches), `(?s).*(decode source failed|unknown key|unknown annotation|project table creations failed).*`)
		})
	}
}

// What the provider cannot carry yet is refused by name, by the host for a
// YAML key it is not claimed and by the provider for a claimed one.
func TestHostRefusesTableDeclarationsTheProviderCannotCarry_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, flag, file, content, want string }{
		{"an unclaimed YAML key", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    uniform_partitions: 4\n    columns:\n      id: {type: Uint64, primary: true}\n", `unknown key "uniform_partitions"`},
		{"a family cache mode", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_families:\n      cold: {cache_mode: regular}\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a zstd family", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_families:\n      cold: {compression: zstd}\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a changefeed in Go", "--root-dir", "entities.go",
			"package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n", `decode Go directives of engine "ydb"`},
		{"a zstd family in Go", "--root-dir", "entities.go",
			"package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"cold\" compression=\"zstd\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n", `decode Go directives of engine "ydb"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(hostSource(c, test.flag, test.file, test.content), qt.Matches, `(?s).*`+test.want+`.*`)
		})
	}
}
