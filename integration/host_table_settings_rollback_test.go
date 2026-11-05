//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A change of a row table's own settings rolls back as the host's embedded
// engine rolls it back: `migrations generate` writes the ALTER TABLE that
// gives the table its settings back (every partitioning setting the table
// held, for a changed partitioning) and, where the forward change sets a TTL,
// the rows it deleted as the limitation; `migrations up` and `migrations
// down` run both files, and the first declaration plans nothing after them.
func TestHostRollsBackTableSettings_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, before, after, statement string
		lostRows                       bool
	}{
		{name: "a changed TTL", before: "P30D", after: "P7D", statement: "ALTER TABLE `{dir}/items` SET (TTL = Interval(\"P30D\") ON `created_at`);", lostRows: true},
		{name: "a removed TTL", before: "P30D", statement: "ALTER TABLE `{dir}/items` SET (TTL = Interval(\"P30D\") ON `created_at`);"},
		{name: "an added TTL", after: "P7D", statement: "ALTER TABLE `{dir}/items` RESET (TTL);", lostRows: true},
		{name: "a changed partitioning", after: "partitions", statement: "ALTER TABLE `{dir}/items` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
			"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			// declared states the TTL of the interval given, the minimum
			// partition count 3 for "partitions", or no setting for "".
			ttl := func(interval string) string {
				return "    platform:\n      ydb: {row_deletion_column: created_at, row_deletion_interval: " + interval + "}\n"
			}
			declared := func(setting string) string {
				stated := map[string]string{"": "", "P30D": ttl("P30D"), "P7D": ttl("P7D"), "partitions": "    auto_partitioning_min_partitions_count: 3\n"}[setting]
				return "tables:\n  items:\n    schema: " + directory + "\n" + stated +
					"    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      created_at: {type: Timestamp}\n"
			}
			hostRun(c, directory, "the table", declared(test.before), `(?s).*Schema apply completed successfully.*`)

			migrations, _, down := hostGenerate(c, directory, "yaml", declared(test.after))
			applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
			settled := hostPlanned(c, directory, "yaml", declared(test.after))
			rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

			c.Assert(down, qt.Contains, strings.ReplaceAll(test.statement, "{dir}", directory))
			c.Assert(strings.Contains(down, "cannot recover rows YDB deleted under the forward TTL"), qt.Equals, test.lostRows)
			c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
			c.Assert(settled, qt.HasLen, 0)
			c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
			c.Assert(hostPlanned(c, directory, "yaml", declared(test.before)), qt.HasLen, 0)
		})
	}
}
