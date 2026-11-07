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
