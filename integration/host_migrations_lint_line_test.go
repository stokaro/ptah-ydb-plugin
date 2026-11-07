//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the YDB rows of the host's
// TestMigrationsLintThroughAProviderOfTheNamedDialect: `migrations lint
// --dialect ydb --server-version` resolves the release line before it asks
// this provider, and the provider judges the files by that line's
// capabilities. A column added with a default needs add_column_with_default,
// which 25.1 lacks and 26.2 has, so YD103 reports it on 25.1 alone. The run
// connects to no database.
func TestHostMigrationsLintJudgesByTheServerVersion_HappyPath(t *testing.T) {
	for _, line := range []struct {
		version string
		want    string
	}{
		{version: "25.1", want: `(?s).*0000000002_v\.up\.sql:1 \[error\] YD103: ADD COLUMN v gives the column a default, which needs target capability ` +
			`add_column_with_default, unavailable on this ydb target.*1 finding\(s\)\..*`},
		{version: "26.2", want: `(?s).*No lint findings\..*`},
	} {
		t.Run(line.version, func(t *testing.T) {
			c := testcheck.New(t)
			migrations := writeFiles(c, c.TempDir(), map[string]string{
				"0000000001_t.up.sql":   "CREATE TABLE t (id Int64 NOT NULL, PRIMARY KEY (id));\n",
				"0000000001_t.down.sql": "DROP TABLE t;\n",
				"0000000002_v.up.sql":   "ALTER TABLE t ADD COLUMN v Int64 DEFAULT 1;\n",
				"0000000002_v.down.sql": "ALTER TABLE t DROP COLUMN v;\n",
			})

			output, err := hostCommand(c, "migrations", "lint", "--dialect", "ydb", "--server-version", line.version, "--dir", migrations, "--fail-on", "none")

			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Matches, line.want)
		})
	}
}

// The host's former TestRunLint_LintsYQLForYDB checks three independent YQL
// findings and the 25.1/26.2 difference through the CLI. The lint run reads
// the migration directory without connecting to a database.
func TestHostMigrationsLintReportsYQLRules_HappyPath(t *testing.T) {
	for _, line := range []struct {
		name, dialect, version string
		want, absent           []string
	}{
		{"26.2", "ydb", "26.2.1.14", []string{"YD101", "YD104"}, []string{"YD103"}},
		{"25.1 TLS alias", "ydbs", "25.1.4.7", []string{"YD101", "YD103", "YD104"}, nil},
	} {
		t.Run(line.name, func(t *testing.T) {
			c := testcheck.New(t)
			migrations := writeFiles(c, c.TempDir(), map[string]string{
				"0000000001_users.up.sql": "CREATE TABLE `shop/users` (id Uint64 NOT NULL, email Utf8, PRIMARY KEY (id), " +
					"INDEX users_email GLOBAL ON (email));\n",
				"0000000001_users.down.sql": "DROP TABLE `shop/users`;\n",
				"0000000002_change.up.sql": "ALTER TABLE `shop/users` ADD COLUMN score Int32 DEFAULT 0;\n" +
					"ALTER TABLE `shop/users` ADD INDEX users_score GLOBAL UNIQUE SYNC ON (score);\n" +
					"ALTER TABLE `shop/users` DROP COLUMN email;\n",
				"0000000002_change.down.sql": "ALTER TABLE `shop/users` DROP COLUMN score;\n",
			})
			out, err := hostCommand(c, "migrations", "lint", "--dialect", line.dialect, "--server-version", line.version,
				"--dir", migrations, "--fail-on", "none")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			for _, code := range line.want {
				c.Assert(out, qt.Contains, code+":", qt.Commentf("%s", out))
			}
			for _, code := range line.absent {
				c.Assert(out, qt.Not(qt.Contains), code+":", qt.Commentf("%s", out))
			}
		})
	}
}
