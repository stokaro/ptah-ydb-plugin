//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// scopedStatements is what `schema diff` from the database to the YAML text
// plans in directory with the selector flags given, sorted, read from its JSON
// report.
func scopedStatements(c *qt.C, directory, text string, flags ...string) []string {
	c.Helper()
	host, manifest := hostManifest(c)
	source := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(source, []byte(text), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 3*time.Minute)
	defer cancel()
	command := testCommand(ctx, host, append([]string{"schema", "diff", "--provider-config", manifest, "--provider-engine", "ydb",
		"--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", source, "--schemas", directory, "--format", "json"}, flags...)...)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	c.Assert(err, qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", output, diagnostics.String()))
	var result struct {
		Statements []string `json:"statements"`
	}
	c.Assert(json.Unmarshal(output, &result), qt.IsNil)
	statements := make([]string, 0, len(result.Statements))
	for _, statement := range result.Statements {
		statements = append(statements, strings.TrimSpace(statement))
	}
	slices.Sort(statements)
	return statements
}

// scopeRow is one selection of a scoped diff and the statements it plans.
type scopeRow struct {
	name  string
	flags []string
	want  []string
}

// runScopeRows runs each row's scoped diff of text in directory.
func runScopeRows(t *testing.T, directory, text string, rows []scopeRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(scopedStatements(c, directory, text, row.flags...), qt.DeepEquals, row.want)
		})
	}
}

// The host selects a YDB topic by its own name in the directory that holds
// it, on both sides of a comparison: an include plans only that topic's
// drop or creation, an exclusion leaves it out of both the declaration and
// the read, so a held topic is neither dropped nor created again, and an
// exclusion of the directory's topics plans nothing. Ported from the host's
// TestScope_Topics and TestExclude_Topics (internal/atlasfilter).
func TestHostScopesTopics_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	for _, name := range []string{"events", "queue"} {
		absolute := path.Join(database, directory, name)
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+absolute+"`"), qt.IsNil)
		c.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+absolute+"`"), qt.IsNil)
		})
	}
	declared := "topics:\n  created:\n    schema: " + directory + "\n"
	topic := func(name string) string { return "`" + directory + "/" + name + "`" }

	runScopeRows(t, directory, declared, []scopeRow{
		{name: "include a held topic", flags: []string{"--include", directory + ".events[type=topic]"}, want: []string{"DROP TOPIC " + topic("events")}},
		{name: "include a declared topic", flags: []string{"--include", directory + ".created[type=topic]"}, want: []string{"CREATE TOPIC " + topic("created")}},
		{name: "exclude by name", flags: []string{"--exclude", directory + ".events[type=topic]"},
			want: []string{"CREATE TOPIC " + topic("created"), "DROP TOPIC " + topic("queue")}},
		{name: "exclude the directory's topics", flags: []string{"--exclude", directory + ".*[type=topic]"}, want: make([]string, 0)},
		{name: "the directory", want: []string{"CREATE TOPIC " + topic("created"), "DROP TOPIC " + topic("events"), "DROP TOPIC " + topic("queue")}},
	})
}

// The host selects a YDB secret by its own name, on both sides: an include
// plans only that secret's drop, an exclusion keeps it, and an exclusion of
// the directory's secrets plans nothing. Ported from the host's
// TestScope_Secrets and TestExclude_Secrets.
func TestHostScopesSecrets_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	skipWithout(c, session, secrets.Capability)
	for _, name := range []string{"pg", "s3"} {
		absolute := path.Join(database, directory, name)
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE SECRET `"+absolute+"` WITH (value = 'probe');"), qt.IsNil)
		c.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = execFixtureSchema(ctx, opened.DB, "DROP SECRET `"+absolute+"`")
		})
	}
	secret := func(name string) string { return "DROP SECRET `" + directory + "/" + name + "`" }

	runScopeRows(t, directory, "secrets: {}\n", []scopeRow{
		{name: "include", flags: []string{"--include", directory + ".pg[type=secret]"}, want: []string{secret("pg")}},
		{name: "exclude by name", flags: []string{"--exclude", directory + ".pg[type=secret]"}, want: []string{secret("s3")}},
		{name: "exclude the directory's secrets", flags: []string{"--exclude", directory + ".*[type=secret]"}, want: make([]string, 0)},
		{name: "the directory", want: []string{secret("pg"), secret("s3")}},
	})
}

// The host selects an external data source and an external table each by
// its own type: an exclusion of the tables plans only the source's drop, and
// an exclusion of the source as well keeps both. Ported from the host's
// TestScope_ExternalObjects and TestExclude_ExternalObjects. The include row
// is left out: an include of the table also drops its data source, which the
// selection keeps as the table's requirement on the read alone (gap G225).
func TestHostScopesExternalObjects_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	source, table := path.Join(database, directory, "s3"), path.Join(database, directory, "files")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE EXTERNAL DATA SOURCE `"+source+
		"` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.test/scope/', AUTH_METHOD = 'NONE')"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE EXTERNAL TABLE `"+table+
		"` (id Int64 NOT NULL) WITH (DATA_SOURCE = '"+source+"', LOCATION = '/', FORMAT = 'json_each_row')"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL TABLE `"+table+"`")
		_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL DATA SOURCE `"+source+"`")
	})
	dropTable, dropSource := "DROP EXTERNAL TABLE `"+directory+"/files`", "DROP EXTERNAL DATA SOURCE `"+directory+"/s3`"

	runScopeRows(t, directory, "external_data_sources: {}\nexternal_tables: {}\n", []scopeRow{
		{name: "exclude the tables", flags: []string{"--exclude", directory + ".*[type=external_table]"}, want: []string{dropSource}},
		{name: "exclude everything external", flags: []string{"--exclude", directory + ".*[type=external_table]", "--exclude", directory + ".s3[type=external_data_source]"},
			want: make([]string, 0)},
		{name: "the directory", want: []string{dropSource, dropTable}},
	})
}

// A grant on a topic leaves a selection with its topic, on both sides: a diff
// that revokes the grants on two topics revokes only the kept topic's when
// the other is excluded, and nothing when the directory's topics are, so a
// selection plans no REVOKE on a topic it left out. Ported from the
// host's TestScope_ObjectGrantsRideTheirObject and
// TestExclude_ObjectGrantsLeaveWithTheirObject.
func TestHostScopesGrantsWithTheirObject_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	group := "ptahscope" + strings.ToLower(directory[len(directory)-8:])
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE GROUP `"+group+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP `"+group+"`"), qt.IsNil)
	})
	for _, name := range []string{"events", "queue"} {
		absolute := path.Join(database, directory, name)
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+absolute+"`"), qt.IsNil)
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "GRANT 'ydb.granular.select_row' ON `"+absolute+"` TO `"+group+"`"), qt.IsNil)
		c.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+absolute+"`"), qt.IsNil)
		})
	}
	declared := "topics:\n  events:\n    schema: " + directory + "\n  queue:\n    schema: " + directory + "\nroles:\n  " + group + ":\n    group: true\n"
	revoke := func(name string) string {
		return "REVOKE 'ydb.granular.select_row' ON `" + directory + "/" + name + "` FROM `" + group + "`"
	}

	runScopeRows(t, directory, declared, []scopeRow{
		// An include names the topics alone, and a grant is no topic: the
		// host's embedded engine planned no REVOKE either (measured on
		// 26.2.1.14).
		{name: "include a topic", flags: []string{"--include", directory + ".events[type=topic]"}, want: make([]string, 0)},
		{name: "exclude a topic", flags: []string{"--exclude", directory + ".queue[type=topic]"}, want: []string{revoke("events")}},
		{name: "exclude the directory's topics", flags: []string{"--exclude", directory + ".*[type=topic]"}, want: make([]string, 0)},
		{name: "the directory", want: []string{revoke("events"), revoke("queue")}},
	})
}
