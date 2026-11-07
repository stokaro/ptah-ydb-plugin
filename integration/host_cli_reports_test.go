//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// vizModel is a model `ptah introspect` writes for a YDB table in a
// directory: YDB type names and a Serial key.
const vizModel = "package models\n\n//ptah:schema:table name=\"orders\" schema=\"shop\"\ntype Order struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"Int64\" not_null=\"true\" primary=\"true\" auto_increment=\"true\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"customer\" type=\"Utf8\" not_null=\"true\"\n\tCustomer string\n" +
	"\t//ptah:schema:field name=\"created\" type=\"Timestamp\"\n\tCreated *string\n}\n"

// A diagram of a YDB model draws its tables in YDB's types, and with
// --security runs the rules under the named line's capabilities: YDB has no
// row-level security, so the rule that needs it says it was not checked
// rather than passing quietly. Ported from the host's
// TestCommand_YDBDialectDrawsAndChecksUnderItsLine (internal/cli/viz).
func TestHostVizDrawsAYDBModelUnderItsLine_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "models"), map[string]string{"orders.go": vizModel})

	out, err := hostCommand(c, "viz", "--root-dir", dir, "--format", "mermaid", "--include-columns", "--security", "--dialect", "ydb", "--server-version", "25.1.4.7")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "  shop_orders {\n    Int64 id PK\n    Utf8 customer\n    Timestamp created\n  }\n")
	c.Assert(out, qt.Contains, "  %% PRV01 not checked here: the target does not model row-level security\n")
}

// rebuiltColumnFiles are two SQL schema files that differ by one column type,
// a change YDB makes only by rebuilding the table.
func rebuiltColumnFiles(c *qt.C) (from, to string) {
	c.Helper()
	dir := c.TempDir()
	from, to = filepath.Join(dir, "from.sql"), filepath.Join(dir, "to.sql")
	c.Assert(os.WriteFile(from, []byte("CREATE TABLE rb (id Int64 NOT NULL, v Int32, PRIMARY KEY (id));\n"), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(to, []byte("CREATE TABLE rb (id Int64 NOT NULL, v Int64, PRIMARY KEY (id));\n"), 0o600), qt.IsNil)
	return from, to
}

// devOnlyURL names the dialect alone: a diff between two files connects to
// nothing, so the server it names is never dialed.
const devOnlyURL = "ydb://127.0.0.1:1/local"

// The native surface asks for a rebuild with --allow-table-rebuild, and a
// diff that needs one names that flag. Ported from the host's
// TestSchemaDiffNamesTheRebuildFlagOnYDB.
func TestHostSchemaDiffNamesTheRebuildFlag_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	from, to := rebuiltColumnFiles(c)

	out, err := hostCommand(c, "schema", "diff", "--from", from, "--to", to, "--dev-url", devOnlyURL)

	var exited *exec.ExitError
	c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, `changing the type of column "v" of table "rb" (Int32 -> Int64), which requires target capability alter_column_type, `+
		"unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild")
}

// The flag is the request: with it, the same diff plans the rebuild. Ported
// from the host's TestSchemaDiffPlansARebuildWithTheFlagOnYDB.
func TestHostSchemaDiffNamesTheRebuildFlag_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	from, to := rebuiltColumnFiles(c)

	out, err := hostCommand(c, "schema", "diff", "--from", from, "--to", to, "--dev-url", devOnlyURL, "--allow-table-rebuild")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "ALTER TABLE `__ptah_rebuild_rb` RENAME TO `rb`;")
}

// A privilege on the database itself is a privilege a role holds, named by
// the path the read found the database at: `schema security` reports a group
// that holds CONNECT on the database and was not observed using it under
// ROL01 with the database as its subject, and `schema inspect --format sql`
// writes the grant ON the database's path. The host's embedded engine wrote
// both the same way (measured on 26.2.1.14). Ported from the host's
// TestROL01_NamesAGrantOnTheDatabase and
// TestSchemaInspectReport_SQLNamesTheDatabaseByTheReadsPath.
func TestHostReportsAGrantOnTheDatabaseByItsPath_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	group := "ptahdbgrant" + strings.ToLower(directory[len(directory)-8:])
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE GROUP `"+group+"`"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "GRANT 'ydb.database.connect' ON `"+database+"` TO `"+group+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "REVOKE 'ydb.database.connect' ON `"+database+"` FROM `"+group+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP `"+group+"`"), qt.IsNil)
	})
	usage := filepath.Join(c.TempDir(), "usage.json")
	c.Assert(os.WriteFile(usage, []byte("[]"), 0o600), qt.IsNil)

	report, err := hostCommand(c, "schema", "security", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--format", "json", "--role-usage", usage, "--fail-on", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", report))
	inspected, err := hostCommand(c, "schema", "inspect", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--format", "sql")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", inspected))

	var findings struct {
		Findings []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Subject struct {
				Kind, Name string
			} `json:"subject"`
		} `json:"findings"`
	}
	c.Assert(json.Unmarshal([]byte(report[strings.Index(report, "{"):]), &findings), qt.IsNil, qt.Commentf("%s", report))
	named := make([]string, 0, len(findings.Findings))
	for _, finding := range findings.Findings {
		named = append(named, finding.Code+" "+finding.Subject.Kind+" "+finding.Subject.Name+": "+finding.Message)
	}
	named = slices.DeleteFunc(named, func(line string) bool {
		return !strings.HasPrefix(line, "ROL01 ") || !strings.Contains(line, group)
	})
	c.Assert(named, qt.DeepEquals, []string{"ROL01 database " + database + ": role " + group + " holds privileges on database " + database + ", which it was not observed using"})
	c.Assert(inspected, qt.Contains, "GRANT 'ydb.database.connect' ON `"+database+"` TO `"+group+"`;")
}

// A migration directory linted for YDB with no dev database names the rule
// that reads a baseline schema as one that ran thinner, rather than passing it
// quietly: DS110P is the one rule whose YDB verdict needs a dev database.
// Ported from the host's TestYDBVerdicts_RulesThatNeedADevDatabaseAreNamedUnmet.
func TestHostMigrationsLintNamesTheRuleThatNeedsADevDatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "migrations"), map[string]string{
		"0000000001_users.up.sql":    "CREATE TABLE `shop/users` (id Int64 NOT NULL, email Utf8, PRIMARY KEY (id));\n",
		"0000000001_users.down.sql":  "DROP TABLE `shop/users`;\n",
		"0000000002_change.up.sql":   "ALTER TABLE `shop/users` DROP COLUMN email;\n",
		"0000000002_change.down.sql": "-- nothing\n",
	})

	out, err := hostCommand(c, "migrations", "lint", "--dir", dir, "--fail-on", "none")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "warning: DS110P ran without the baseline schema it reads, so this analysis is thinner than the same directory "+
		"would get against a dev database the run can read")
}

// unscopedLint lints the Atlas directory holding 1_t.sql with sql, naming no
// dialect and selecting this provider for ydb, from the directory's parent.
func unscopedLint(c *qt.C, sql string) string {
	c.Helper()
	root := c.TempDir()
	writeFiles(c, filepath.Join(root, "migrations"), map[string]string{"1_t.sql": sql})
	host, manifest := hostManifest(c)
	command := testCommand(c.Context(), host, "migrations", "lint", "--dir", "migrations", "--dir-format", "atlas", "--fail-on", "none", "--provider-config", manifest)
	command.Dir = root
	out, err := command.CombinedOutput()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	return string(out)
}

// A lint run that names no dialect reads the text with the dialect-blind
// scanner, and names the statements the YDB owner splits differently, here
// the DROPs after a YQL string that ends in a backslash, as statements it did
// not judge. Ported from the host's
// TestWriteUnmetInputNoticeNamesUnjudgedStatements.
func TestHostMigrationsLintNamesTheStatementsYDBDidNotJudge_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	out := unscopedLint(c, "SELECT 'a\\';\nDROP TOPIC t;\nSELECT '';\nDROP TOPIC u;\nSELECT '';\n")

	c.Assert(out, qt.Contains, "warning: the ydb owner did not judge migrations/1_t.sql:2, migrations/1_t.sql:4: its own lexing splits the text "+
		"differently than the dialect-blind scanner this run used, so the rules that read what only ydb recognizes did not check them; "+
		"name the dialect to read the text one way")
}

// A text every reader splits alike gets no such notice. Ported from the same
// host test.
func TestHostMigrationsLintJudgesATextEveryReaderSplitsAlike(t *testing.T) {
	c := testcheck.New(t)

	out := unscopedLint(c, "DROP TOPIC t;\n")

	c.Assert(out, qt.Not(qt.Contains), "did not judge")
}
