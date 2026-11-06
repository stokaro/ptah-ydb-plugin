//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The directories testdata/builtinhistory recorded the built-in YDB engine's
// migration state in. The recorded checksums cover migration files that name
// them, so the tests use these names rather than ones they generate.
const (
	builtinHistoryDirectory = "ptah_builtin_history"
	builtinAtlasDirectory   = "ptah_builtin_history_atlas"
	builtinHistoryData      = "testdata/builtinhistory"
)

// builtinHistoryTables are the tables testdata/builtinhistory holds rows
// of, each with its columns and their YQL types in the engine's layout.
var builtinHistoryTables = []struct {
	table   string
	columns [][2]string
}{
	{builtinHistoryDirectory + "/schema_migrations", [][2]string{{"version", "Int64"}, {"description", "Utf8"}, {"applied_at", "Timestamp"},
		{"state", "Utf8"}, {"applied", "Int64"}, {"total", "Int64"}, {"error", "Utf8?"}, {"error_stmt", "Utf8?"}, {"execution_time_ms", "Int64"},
		{"checksum", "Utf8"}}},
	{builtinHistoryDirectory + "/schema_migrations_log", [][2]string{{"run_id", "Utf8"}, {"seq", "Int64"}, {"operation", "Utf8"}, {"version", "Int64"},
		{"state", "Utf8"}, {"actor", "Utf8?"}, {"actor_source", "Utf8"}, {"checksum", "Utf8?"}, {"logged_at", "Timestamp"}, {"error", "Utf8?"}}},
	{builtinHistoryDirectory + "/ptah_migration_tags", [][2]string{{"tag", "Utf8"}, {"version", "Int64"}, {"recorded_at", "Timestamp"}}},
	{builtinAtlasDirectory + "/atlas_schema_revisions", [][2]string{{"version", "Utf8"}, {"description", "Utf8"}, {"type", "Int64"},
		{"applied", "Int64"}, {"total", "Int64"}, {"executed_at", "Timestamp"}, {"execution_time", "Int64"}, {"error", "Utf8?"},
		{"error_stmt", "Utf8?"}, {"hash", "Utf8"}, {"partial_hashes", "Utf8?"}, {"operator_version", "Utf8"}}},
}

// recordedRows are the rows of testdata/builtinhistory/<name>.jsonl, each as
// the JSON object ydb printed, numbers kept as written.
func recordedRows(c *qt.C, name string) []map[string]any {
	c.Helper()
	body, err := os.ReadFile(filepath.Join(builtinHistoryData, name+".jsonl"))
	c.Assert(err, qt.IsNil)
	var rows []map[string]any
	lines := bufio.NewScanner(bytes.NewReader(body))
	for lines.Scan() {
		decoder := json.NewDecoder(strings.NewReader(lines.Text()))
		decoder.UseNumber()
		row := make(map[string]any)
		c.Assert(decoder.Decode(&row), qt.IsNil)
		rows = append(rows, row)
	}
	c.Assert(lines.Err(), qt.IsNil)
	return rows
}

// recordedValue is a recorded JSON value as the YQL type of its column takes
// it.
func recordedValue(c *qt.C, kind string, value any) types.Value {
	c.Helper()
	if strings.HasSuffix(kind, "?") {
		if value == nil {
			return types.NullValue(types.TypeText)
		}
		return types.OptionalValue(types.TextValue(value.(string)))
	}
	switch kind {
	case "Int64":
		number, err := value.(json.Number).Int64()
		c.Assert(err, qt.IsNil)
		return types.Int64Value(number)
	case "Timestamp":
		at, err := time.Parse(time.RFC3339Nano, value.(string))
		c.Assert(err, qt.IsNil)
		return types.TimestampValueFromTime(at)
	default:
		return types.TextValue(value.(string))
	}
}

// builtinHistory makes the database hold the migration state the built-in
// engine recorded in testdata/builtinhistory: its tables, created with its
// statements, and its rows. The directories are removed before and after.
func builtinHistory(c *qt.C, opened *connection.Connection, database string) {
	c.Helper()
	remove := func(ctx context.Context) {
		for _, directory := range []string{builtinHistoryDirectory, builtinAtlasDirectory} {
			removeTree(ctx, opened, path.Join(database, directory))
		}
	}
	remove(c.Context())
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		remove(ctx)
	})
	statements, err := os.ReadFile(filepath.Join(builtinHistoryData, "tables.yql"))
	c.Assert(err, qt.IsNil)
	for statement := range strings.SplitSeq(string(statements), ";\n") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		c.Assert(execFixtureSchema(c.Context(), opened.DB, strings.ReplaceAll(statement, "`ptah_builtin", "`"+database+"/ptah_builtin")), qt.IsNil)
	}
	for _, table := range builtinHistoryTables {
		for _, row := range recordedRows(c, path.Base(table.table)) {
			declarations, names, parameters, values := make([]string, 0), make([]string, 0), make([]string, 0), make([]any, 0)
			for _, column := range table.columns {
				declarations = append(declarations, "DECLARE $"+column[0]+" AS "+column[1]+";")
				names = append(names, "`"+column[0]+"`")
				parameters = append(parameters, "$"+column[0])
				values = append(values, sql.Named(column[0], recordedValue(c, column[1], row[column[0]])))
			}
			// #nosec G202 -- a fixed fixture table under the database root, and the recorded columns
			_, err := opened.DB.ExecContext(c.Context(), strings.Join(declarations, " ")+" UPSERT INTO `"+path.Join(database, table.table)+"` ("+
				strings.Join(names, ", ")+") VALUES ("+strings.Join(parameters, ", ")+")", values...)
			c.Assert(err, qt.IsNil)
		}
	}
}

// builtinMigrations is a copy of the recorded native migration directory
// whose second migration no longer fails, as a user fixes the file before a
// resume, with a third migration after it.
func builtinMigrations(c *qt.C) string {
	c.Helper()
	dir := c.TempDir()
	for _, name := range []string{"1_create.up.sql", "1_create.down.sql", "2_more.up.sql", "2_more.down.sql"} {
		body, err := os.ReadFile(filepath.Join(builtinHistoryData, "migrations", name))
		c.Assert(err, qt.IsNil)
		c.Assert(os.WriteFile(filepath.Join(dir, name), body, 0o600), qt.IsNil) // #nosec G703 -- a fixed file name under the test's c.TempDir()
	}
	return writeFiles(c, dir, map[string]string{
		"2_more.up.sql":    "CREATE TABLE `ptah_builtin_history/more` (id Int64 NOT NULL, PRIMARY KEY (id));\nINSERT INTO `ptah_builtin_history/items` (id) VALUES (1);\n",
		"3_third.up.sql":   "CREATE TABLE `ptah_builtin_history/third` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"3_third.down.sql": "DROP TABLE `ptah_builtin_history/third`;\n",
	})
}

// builtinRevision is one row of the engine's native revision table as text,
// in the column order of its layout.
func builtinRevision(c *qt.C, opened *connection.Connection, database string, version int64) string {
	c.Helper()
	var description, at, state, checksum string
	var applied, total, milliseconds int64
	var failure, statement sql.NullString
	// #nosec G202 -- a fixed fixture table under the database root
	err := opened.DB.QueryRowContext(c.Context(), "SELECT description, CAST(applied_at AS Utf8), state, applied, total, error, error_stmt, "+
		"execution_time_ms, checksum FROM `"+path.Join(database, builtinHistoryDirectory, "schema_migrations")+"` WHERE version = "+
		fmt.Sprintf("%dl", version)).Scan(&description, &at, &state, &applied, &total, &failure, &statement, &milliseconds, &checksum)
	c.Assert(err, qt.IsNil)
	return fmt.Sprintf("%s %s %s %d/%d %v %v %d %s", description, at, state, applied, total, failure.Valid, statement.Valid, milliseconds, checksum)
}

// A database the built-in YDB engine migrated holds its revision, log and
// tag tables in that engine's layout. Through the provider the host reads
// them as the engine did: the second migration is dirty, stopped after one
// of its two statements, and the tag names version 1. A resume with
// --allow-dirty completes it, the next migration applies and is rolled
// back, a tag is added and one removed, and every row is written in the
// engine's layout: the engine's own row of version 1 is left as it wrote it.
func TestHostContinuesTheMigrationsTheBuiltInEngineRan_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, database := lockFixture(c)
	builtinHistory(c, opened, database)
	recorded := recordedRows(c, "schema_migrations")[0]
	migrations := builtinMigrations(c)
	scope := []string{"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-schema", builtinHistoryDirectory}
	first := builtinRevision(c, opened, database, 1)

	status, statusErr := hostCommand(c, append([]string{"migrations", "status", "--migrations-dir", migrations}, scope...)...)
	tags, tagsErr := hostCommand(c, append([]string{"migrations", "tag"}, scope...)...)
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--migrations-dir", migrations, "--allow-dirty"}, scope...)...)
	rolledBack, downErr := hostCommand(c, append([]string{"migrations", "down", "--migrations-dir", migrations, "--target", "2", "--confirm"}, scope...)...)
	tagged, tagErr := hostCommand(c, append([]string{"migrations", "tag", "v2"}, scope...)...)
	untagged, untagErr := hostCommand(c, append([]string{"migrations", "tag", "v1", "--delete"}, scope...)...)
	retagged, retagsErr := hostCommand(c, append([]string{"migrations", "tag"}, scope...)...)

	c.Assert(statusErr, qt.IsNil, qt.Commentf("migrations status:\n%s", status))
	c.Assert(status, qt.Contains, "Dirty Migration: version=2 state=failed direction=up applied=1/2")
	c.Assert(tagsErr, qt.IsNil, qt.Commentf("migrations tag:\n%s", tags))
	c.Assert(tags, qt.Matches, `(?s).*\nv1\s+1\s+`+recordedRows(c, "ptah_migration_tags")[0]["recorded_at"].(string)[:19]+`Z\n.*`)
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("migrations up --allow-dirty:\n%s", resumed))
	c.Assert(resumed, qt.Contains, "Database is now at version: 3")
	c.Assert(downErr, qt.IsNil, qt.Commentf("migrations down:\n%s", rolledBack))
	c.Assert(tagErr, qt.IsNil, qt.Commentf("migrations tag v2:\n%s", tagged))
	c.Assert(untagErr, qt.IsNil, qt.Commentf("migrations tag v1 --delete:\n%s", untagged))
	c.Assert(retagsErr, qt.IsNil, qt.Commentf("migrations tag:\n%s", retagged))
	c.Assert(retagged, qt.Matches, `(?s)TAG\s+VERSION\s+RECORDED\nv2\s+2\s+\S+\n`)
	c.Assert(builtinRevision(c, opened, database, 1), qt.Equals, first)
	c.Assert(first, qt.Contains, recorded["checksum"].(string))
	c.Assert(builtinRevision(c, opened, database, 2), qt.Matches, `More \S+ applied 2/2 false false \d+ [0-9a-f]{64}`)
	_, thirdAfter := heldEntries(c, opened, path.Join(database, builtinHistoryDirectory))["third"]
	c.Assert(thirdAfter, qt.IsFalse)
}

// The revision, log and tag tables the built-in YDB engine wrote are left out
// of a schema read, as that engine left them out: a plan for the directory
// that holds them, from a declaration of the directory's own tables, is
// empty, where a read that described them would drop the record of every
// migration the database ran.
func TestHostLeavesTheBuiltInEnginesMigrationTablesOutOfAPlan_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, database := lockFixture(c)
	builtinHistory(c, opened, database)
	declared := writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": "tables:\n" +
		"  items:\n    schema: ptah_builtin_history\n    columns:\n      id: {type: Int64, primary: true}\n" +
		"  more:\n    schema: ptah_builtin_history\n    columns:\n      id: {type: Int64, primary: true}\n"})
	atlas := writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": "tables:\n" +
		"  items:\n    schema: ptah_builtin_history_atlas\n    columns:\n      id: {type: Int64, primary: true}\n"})

	native, nativeErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", builtinHistoryDirectory,
		"--schema-file", filepath.Join(declared, "schema.yaml"), "--exit-code")
	compat, compatErr := hostCommand(c, "schema", "compare", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", builtinAtlasDirectory,
		"--schema-file", filepath.Join(atlas, "schema.yaml"), "--exit-code")

	c.Assert(nativeErr, qt.IsNil, qt.Commentf("schema compare:\n%s", native))
	c.Assert(native, qt.Contains, "No schema differences detected.")
	c.Assert(compatErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compat))
	c.Assert(compat, qt.Contains, "No schema differences detected.")
}

// The Atlas revision table `ptah-compat migrate apply` of the built-in YDB
// engine wrote reads through the provider as that engine read it: one file
// executed and the next one pending. Applying it writes the new revision in
// the engine's layout and leaves the engine's own row as it wrote it.
func TestHostCompatContinuesTheBuiltInEnginesAtlasRevisions_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	binary, manifest := hostCompat(c)
	opened, _, database := lockFixture(c)
	builtinHistory(c, opened, database)
	dir := c.TempDir()
	recorded, err := os.ReadFile(filepath.Join(builtinHistoryData, "atlas", "20260101000000_create.sql"))
	c.Assert(err, qt.IsNil)
	compatFile(c, dir, "20260101000000_create.sql", string(recorded))
	compatFile(c, dir, "20260102000000_second.sql", "CREATE TABLE `ptah_builtin_history_atlas/second` (id Int64 NOT NULL, PRIMARY KEY (id));\n")
	directory := "file://" + filepath.ToSlash(dir)
	_, hashErr, hashExit := runCompatBinary(c, binary, manifest, nil, "migrate", "hash", "--dir", directory)
	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s", hashErr))
	target := []string{"--url", os.Getenv("PTAH_YDB_TEST_URL"), "--dir", directory, "--revisions-schema", builtinAtlasDirectory}
	revisions := path.Join(database, builtinAtlasDirectory, "atlas_schema_revisions")
	var firstHash string
	// #nosec G202 -- a fixed fixture table under the database root
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT hash FROM `"+revisions+"` WHERE version = '20260101000000'u").Scan(&firstHash), qt.IsNil)

	status, statusErr, statusExit := runCompatBinary(c, binary, manifest, nil, append([]string{"migrate", "status"}, target...)...)
	applied, applyErr, applyExit := runCompatBinary(c, binary, manifest, nil, append([]string{"migrate", "apply"}, target...)...)
	var count uint64
	var held string
	// #nosec G202 -- a fixed fixture table under the database root
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+revisions+"`").Scan(&count), qt.IsNil)
	// #nosec G202 -- a fixed fixture table under the database root
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT hash FROM `"+revisions+"` WHERE version = '20260101000000'u").Scan(&held), qt.IsNil)

	c.Assert(statusExit, qt.Equals, 0, qt.Commentf("migrate status:\n%s%s", status, statusErr))
	c.Assert(status, qt.Contains, "-- Current Version: 20260101000000")
	c.Assert(status, qt.Contains, "-- Pending Files:   1")
	c.Assert(applyExit, qt.Equals, 0, qt.Commentf("migrate apply:\n%s%s", applied, applyErr))
	c.Assert(applied, qt.Contains, "Migration complete. Current version: 20260102000000")
	c.Assert(count, qt.Equals, uint64(2))
	c.Assert(held, qt.Equals, firstHash)
	c.Assert(firstHash, qt.Equals, recordedRows(c, "atlas_schema_revisions")[0]["hash"].(string))
}
