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
	host, manifest := hostManifest(c)
	directory := c.TempDir()
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

// hostManifest returns the pinned host binary and a provider manifest that
// selects this provider for ydb. The test is skipped without the host binary.
func hostManifest(c *qt.C) (string, string) {
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
	manifest := filepath.Join(c.TempDir(), "providers.json")
	c.Assert(os.WriteFile(manifest, encoded, 0o600), qt.IsNil)
	return host, manifest
}

const (
	hostYAMLTable = "tables:\n  host_source_items:\n    auto_partitioning_by_load: ENABLED\n    auto_partitioning_min_partitions_count: 2\n    read_replicas_settings: ANY_AZ:1\n" +
		"    column_families:\n      default: {compression: lz4}\n    columns:\n      id: {type: Uint64, primary: true}\n      body: {type: Utf8}\n"
	hostGoTable = "package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"default\" compression=\"lz4\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n"
	// hostGoAttributes writes the settings as attributes of the host's own table directive.
	hostGoAttributes = "package entities\n\n//ptah:schema:table name=\"host_source_items\" auto_partitioning_by_load=\"ENABLED\" auto_partitioning_min_partitions_count=\"2\" read_replicas_settings=\"ANY_AZ:1\" store=\"ROW\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n\t//ptah:schema:field name=\"created_at\" type=\"Timestamp\"\n\tCreatedAt int64\n}\n"
)

const (
	// hostYAMLPlatformTTL writes the TTL in the ydb group of the table's platform key.
	hostYAMLPlatformTTL = "tables:\n  host_source_items:\n    platform:\n      mysql: {engine: InnoDB}\n      ydb: {row_deletion_column: created_at, row_deletion_interval: P30D}\n" +
		"    columns:\n      id: {type: Uint64, primary: true}\n      created_at: {type: Timestamp}\n"
	// hostGoFamilyFields puts a column in a family.
	hostGoFamilyFields = "package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"cold\" fields=\"body\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody string\n}\n"
)

// goTable is a Go file with one table that writes attributes on its table and
// index directives.
func goTable(directives, index string) string {
	return "package entities\n\n" + directives + "\ntype Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"created_at\" type=\"Timestamp\"\n\tCreatedAt int64\n\t" + index + "\n\tBody string\n}\n"
}

// The host hands the provider a table's partitioning and column families, and
// the provider accepts them: whatever the host reports next, it is not a
// refusal of the declaration.
func TestHostSendsTableDeclarationsToTheProvider_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, flag, file, content string }{
		{"YAML keys", "--schema-file", "schema.yaml", hostYAMLTable},
		{"a Go directive", "--root-dir", "entities.go", hostGoTable},
		{"Go attributes of the table directive", "--root-dir", "entities.go", hostGoAttributes},
		{"a Go TTL", "--root-dir", "entities.go", goTable("//ptah:schema:table name=\"host_source_items\" platform.ydb.row_deletion_column=\"created_at\" platform.ydb.row_deletion_interval=\"P30D\"", "")},
		{"a YAML TTL in the platform group", "--schema-file", "schema.yaml", hostYAMLPlatformTTL},
		{"the columns of a Go family", "--root-dir", "entities.go", hostGoFamilyFields},
		{"a uniform starting layout in YAML", "--schema-file", "schema.yaml", "tables:\n  host_source_items:\n    uniform_partitions: 4\n    columns:\n      id: {type: Uint64, primary: true}\n"},
		{"starting split points in YAML", "--schema-file", "schema.yaml", "tables:\n  host_source_items:\n    partition_at_keys: \"10, 20\"\n    columns:\n      id: {type: Uint64, primary: true}\n"},
		{"a uniform starting layout in Go", "--root-dir", "entities.go", goTable("//ptah:schema:table name=\"host_source_items\" uniform_partitions=\"4\"", "")},
		{"starting split points in Go", "--root-dir", "entities.go", goTable("//ptah:schema:table name=\"host_source_items\" partition_at_keys=\"10, 20\"", "")},
		{"a column table in Go", "--root-dir", "entities.go",
			goTable("//ptah:schema:table name=\"host_source_items\" store=\"COLUMN\" partition_by_hash=\"id\" column_shards=\"2\"", "")},
		{"a column table in YAML", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_store: {hash_columns: [id], partitions: 2}\n    columns:\n      id: {type: Uint64, primary: true}\n      body: {type: Utf8}\n"},
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
		{"a changefeed without its mode in YAML", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    changefeeds:\n      updates: {format: JSON}\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a family cache mode", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_families:\n      cold: {cache_mode: regular}\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a zstd family", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_families:\n      cold: {compression: zstd}\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a consumer of a changefeed the table does not declare in Go", "--root-dir", "entities.go",
			"package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:changefeed:consumer name=\"billing\" changefeed=\"updates\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n", `decode Go directives of engine "ydb"`},
		{"both starting layouts in Go", "--root-dir", "entities.go",
			goTable("//ptah:schema:table name=\"host_source_items\" uniform_partitions=\"4\" partition_at_keys=\"10\"", ""), `decode Go directives of engine "ydb"`},
		{"both starting layouts in YAML", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    uniform_partitions: 4\n    partition_at_keys: \"10\"\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a column table beside row-table partitioning in Go", "--root-dir", "entities.go",
			goTable("//ptah:schema:table name=\"host_source_items\" store=\"COLUMN\" auto_partitioning_by_load=\"ENABLED\"", ""), `decode Go directives of engine "ydb"`},
		{"a column table beside row-table partitioning in YAML", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_store: {}\n    auto_partitioning_by_load: ENABLED\n    columns:\n      id: {type: Uint64, primary: true}\n", `decode source failed`},
		{"a TTL interval in months in Go", "--root-dir", "entities.go",
			goTable("//ptah:schema:table name=\"host_source_items\" platform.ydb.row_deletion_column=\"created_at\" platform.ydb.row_deletion_interval=\"P1M\"", ""), `decode Go directives of engine "ydb"`},
		{"an index option in Go", "--root-dir", "entities.go",
			goTable("//ptah:schema:table name=\"host_source_items\"", "//ptah:schema:index name=\"by_body\" fields=\"body\" auto_partitioning_by_load=\"ENABLED\""), `decode Go directives of engine "ydb"`},
		{"the columns of a family in YAML", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    column_families:\n      cold: {fields: [body]}\n    columns:\n      id: {type: Uint64, primary: true}\n      body: {type: Utf8}\n", `decode source failed`},
		{"a platform property of the ydb group the provider does not read", "--schema-file", "schema.yaml",
			"tables:\n  host_source_items:\n    platform:\n      ydb: {row_deletion_column: created_at, row_deletion_interval: PT1H, other: x}\n    columns:\n      id: {type: Uint64, primary: true}\n      created_at: {type: Timestamp}\n", `decode source failed`},
		{"a column the family names and its struct lacks", "--root-dir", "entities.go",
			"package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"cold\" fields=\"missing\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n", `missing`},
		{"the columns of a family of another table", "--root-dir", "entities.go",
			"package entities\n\n//ptah:schema:table name=\"host_source_items\"\n//ptah:schema:columnfamily name=\"cold\" table=\"host_source_orders\" fields=\"body\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n}\n\n//ptah:schema:table name=\"host_source_orders\"\n" +
				"type Order struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody string\n}\n", `column "body" of struct Item is not declared in this file`},
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

const (
	hostValidTable = "tables:\n  host_source_items:\n    columns:\n      id: {type: Uint64, primary: true}\n      body: {type: Utf8}\n"
	// The two tables are valid alone and cannot both exist: YDB keeps one object
	// at a path and a table holds no children. Only whole-schema validation
	// sees the pair.
	hostNestedTables = "tables:\n  a:\n    columns:\n      id: {type: Uint64, primary: true}\n" +
		"  b:\n    schema: a\n    columns:\n      id: {type: Uint64, primary: true}\n"
)

// The host asks the provider to validate the desired schema before it plans,
// because the provider advertises schema.validate. Whatever the plan reports
// next, it does not stop for want of a validation service.
func TestHostValidatesTheDesiredSchemaWithTheProvider_HappyPath(t *testing.T) {
	c := qt.New(t)
	output := hostSource(c, "--schema-file", "schema.yaml", hostValidTable)
	c.Assert(output, qt.Not(qt.Matches), `(?s).*(no schema validation service|lies inside table).*`)
}

// A refusal by the provider's validation reaches the user with the provider's
// message.
func TestHostReportsTheProvidersValidationRefusal_FailurePath(t *testing.T) {
	c := qt.New(t)
	output := hostSource(c, "--schema-file", "schema.yaml", hostNestedTables)
	c.Assert(output, qt.Matches, `(?s).*YDB table a/b lies inside table a; a table holds no children.*`)
	c.Assert(output, qt.Not(qt.Matches), `(?s).*no schema validation service.*`)
}
