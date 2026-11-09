//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// renderYQLFile runs the pinned host's offline `schema render` over the YQL
// file at file with this provider selected for ydb, and returns what it
// writes to standard output: the statements, without the notes it prints.
func renderYQLFile(c *qt.C, file string) string {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--provider-config", manifest, "--provider-engine", "ydb")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s\n%s", stdout.String(), stderr.String()))
	return stdout.String()
}

// The host renders each YQL schema of the host's TestParseRender corpus
// offline through this provider, reads the render back as a YQL schema and
// renders it again to the same text, and the render holds the statement the
// built-in YDB engine at host 74de0b65b rendered for it. A vector, a
// full-text and a local index are among them: the provider refused the first
// when it read the schema, and the other two when it rendered the table,
// where the built-in engine rendered each one (G239). The spellings differ
// in one place: the built-in engine wrote a split point of a Utf8 key with
// the u suffix, and this provider writes the string, which YDB accepts for
// the key alike on 25.1.4.7 and 26.2.1.14.
func TestHostRendersYQLToAFixedPoint_HappyPath(t *testing.T) {
	const feedTable = "CREATE TABLE events (id Int64 NOT NULL, PRIMARY KEY (id));"
	for _, test := range []struct {
		name, text, want string
	}{
		{name: "a changefeed's consumer", text: feedTable + "ALTER TABLE events ADD CHANGEFEED updates WITH (mode = 'NEW_AND_OLD_IMAGES', format = 'JSON');" +
			"ALTER TOPIC `events/updates` ADD CONSUMER worker WITH (read_from=Timestamp('2026-01-01T00:00:00Z'), availability_period=Interval('PT1H'));",
			want: "ALTER TOPIC `events/updates` ADD CONSUMER `worker` WITH (read_from = Timestamp('2026-01-01T00:00:00Z'), availability_period = Interval('PT1H'));"},
		{name: "a changefeed's settings", text: feedTable + "ALTER TABLE events ADD CHANGEFEED updates WITH (mode='UPDATES', format='JSON', virtual_timestamps=TRUE, " +
			"resolved_timestamps=Interval('PT1S'), retention_period=Interval('PT12H'), initial_scan=TRUE);",
			want: "ALTER TABLE `events` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON', VIRTUAL_TIMESTAMPS = TRUE, RESOLVED_TIMESTAMPS = Interval('PT1S'), " +
				"RETENTION_PERIOD = Interval('PT12H'), INITIAL_SCAN = TRUE);"},
		{name: "a topic's consumer", text: "CREATE TOPIC events; ALTER TOPIC events ADD CONSUMER worker;", want: "CREATE TOPIC `events` (CONSUMER `worker`);"},
		{name: "a coordination node", text: "CREATE COORDINATION NODE locks;", want: "CREATE COORDINATION NODE `locks`;"},
		{name: "a coordination node's settings", text: "CREATE COORDINATION NODE locks WITH (self_check_period = Interval('PT1S'), session_grace_period = Interval('PT2S'), " +
			"read_consistency_mode = 'strict', attach_consistency_mode = 'relaxed', rate_limiter_counters_mode = 'detailed');",
			want: "CREATE COORDINATION NODE `locks` WITH (self_check_period = Interval('PT1S'), session_grace_period = Interval('PT2S'), read_consistency_mode = 'strict', " +
				"attach_consistency_mode = 'relaxed', rate_limiter_counters_mode = 'detailed');"},
		{name: "a view and a topic", text: "CREATE VIEW v WITH (security_invoker = TRUE) AS SELECT 1 AS id; CREATE TOPIC events;",
			want: "CREATE VIEW `v` WITH (security_invoker = TRUE) AS\nSELECT 1 AS id\n;"},
		{name: "a view of a DO block", text: "CREATE VIEW v WITH (security_invoker = TRUE) AS DO BEGIN $f = ($x) -> { RETURN $x + 1; }; SELECT $f(1) AS id; END DO;",
			want: "CREATE VIEW `v` WITH (security_invoker = TRUE) AS\nDO BEGIN $f = ($x) -> { RETURN $x + 1; }; SELECT $f(1) AS id; END DO\n;"},
		{name: "a topic's settings", text: `CREATE TOPIC events (CONSUMER worker WITH (important = true, read_from = Timestamp("2026-01-01T00:00:00Z"), supported_codecs = "raw,gzip")) ` +
			`WITH (min_active_partitions = 2, retention_period = Interval("P1D"), supported_codecs = "raw,gzip");`,
			want: "CREATE TOPIC `events` (CONSUMER `worker` WITH (important = TRUE, read_from = Timestamp('2026-01-01T00:00:00Z'), supported_codecs = 'raw,gzip')) " +
				"WITH (min_active_partitions = 2, retention_period = Interval('P1D'), supported_codecs = 'raw,gzip');"},
		{name: "a topic's partitioning", text: `CREATE TOPIC events (CONSUMER reader WITH (availability_period = Interval("PT1H"))) WITH (min_active_partitions = 2, ` +
			`max_active_partitions = 4, auto_partitioning_strategy = "scale_up", auto_partitioning_up_utilization_percent = 80, auto_partitioning_down_utilization_percent = 20, ` +
			`auto_partitioning_stabilization_window = Interval("PT1M"), partition_write_speed_bytes_per_second = 1024, partition_write_burst_bytes = 2048);`,
			want: "CREATE TOPIC `events` (CONSUMER `reader` WITH (availability_period = Interval('PT1H'))) WITH (min_active_partitions = 2, max_active_partitions = 4, " +
				"auto_partitioning_strategy = 'scale_up', auto_partitioning_up_utilization_percent = 80, auto_partitioning_down_utilization_percent = 20, " +
				"auto_partitioning_stabilization_window = Interval('PT1M'), partition_write_speed_bytes_per_second = 1024, partition_write_burst_bytes = 2048);"},
		{name: "families and a TTL", text: `CREATE TABLE t (id Uint64 NOT NULL, ts Timestamp, body Utf8 FAMILY payload, PRIMARY KEY (id), FAMILY payload (COMPRESSION = "lz4"), ` +
			`FAMILY empty ()) WITH (TTL = Interval("PT1H") ON ts);`,
			want: "    FAMILY `empty` (),\n    FAMILY `payload` (COMPRESSION = 'lz4')\n) WITH (TTL = Interval(\"PT1H\") ON `ts`);"},
		{name: "a TTL in seconds", text: `CREATE TABLE t (id Uint64 NOT NULL, ts Uint64, PRIMARY KEY (id)) WITH (TTL = Interval("PT1H") ON ts AS SECONDS);`,
			want: ") WITH (TTL = Interval(\"PT1H\") ON `ts` AS SECONDS);"},
		{name: "a column table's TTL", text: `CREATE TABLE t (ts Timestamp NOT NULL, id Uint64, PRIMARY KEY (ts)) PARTITION BY HASH (ts) WITH (STORE = COLUMN, TTL = Interval("PT1H") DELETE ON ts);`,
			want: ") PARTITION BY HASH (`ts`) WITH (STORE = COLUMN, TTL = Interval(\"PT3600S\") DELETE ON `ts`);"},
		{name: "a split point", text: `CREATE TABLE t (id Utf8 NOT NULL, PRIMARY KEY (id)) WITH (PARTITION_AT_KEYS = ('a b'u));`,
			want: ") WITH (PARTITION_AT_KEYS = (('a b')));"},
		{name: "split points of two columns", text: `CREATE TABLE t (id Uint64 NOT NULL, part Utf8 NOT NULL, PRIMARY KEY (id, part)) WITH (PARTITION_AT_KEYS = ((10, 'a\n\'b'u), (20)));`,
			want: `) WITH (PARTITION_AT_KEYS = ((10, 'a\n\'b'), (20)));`},
		{name: "a default and a global index", text: "--!syntax_v1\nCREATE TABLE t (id Int64 NOT NULL, value Utf8 DEFAULT 'active'u, PRIMARY KEY (id), INDEX i GLOBAL SYNC ON (value));",
			want: "    `value` Utf8 DEFAULT 'active'u,\n    PRIMARY KEY (`id`),\n    INDEX `i` GLOBAL SYNC ON (`value`)\n);"},
		{name: "a vector index", text: "CREATE TABLE t (id Uint64 NOT NULL, v String, PRIMARY KEY (id), INDEX i GLOBAL SYNC USING vector_kmeans_tree ON (v) " +
			"WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2));",
			want: "    INDEX `i` GLOBAL USING vector_kmeans_tree ON (`v`) WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2)\n);"},
		{name: "a full-text index", text: "CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id), INDEX i GLOBAL USING fulltext_plain ON (v) " +
			"WITH (tokenizer = 'standard', use_filter_lowercase = true));",
			want: "    INDEX `i` GLOBAL USING fulltext_plain ON (`v`) WITH (tokenizer=standard, use_filter_lowercase=true)\n);"},
		{name: "a local index", text: "CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id), INDEX i LOCAL USING bloom_filter ON (v)) PARTITION BY HASH (id) WITH (STORE = COLUMN);",
			want: "    INDEX `i` LOCAL USING bloom_filter ON (`v`) WITH (false_positive_probability = 0.1)\n) PARTITION BY HASH (`id`) WITH (STORE = COLUMN);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			files := writeFiles(c, c.TempDir(), map[string]string{"schema.sql": test.text})

			first := renderYQLFile(c, filepath.Join(files, "schema.sql"))
			c.Assert(os.WriteFile(filepath.Join(files, "again.sql"), []byte(first), 0o600), qt.IsNil)
			second := renderYQLFile(c, filepath.Join(files, "again.sql"))

			c.Assert(first, qt.Contains, test.want)
			c.Assert(second, qt.Equals, first)
		})
	}
}

// yqlIndexRun is one run of a YQL schema: its text, the text the run's
// preview holds, and the ALTER TABLE statements its plan runs.
type yqlIndexRun struct {
	name, text, holds string
	want              []string
}

// applyYQLIndexRuns applies each run's YQL schema to directory in turn, with
// {dir} standing for the directory, and checks what the run planned.
func applyYQLIndexRuns(c *qt.C, directory string, runs []yqlIndexRun) {
	c.Helper()
	for _, run := range runs {
		root := writeSchema(c, map[string]string{"schema.sql": replaceDirectory(run.text, directory)})
		output, err := hostApplySQL(c, directory, filepath.Join(root, "schema.sql"))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Contains, run.holds, qt.Commentf("%s", run.name))
		c.Assert(statements(output, directory), qt.DeepEquals, run.want, qt.Commentf("%s: %s", run.name, output))
	}
}

// The host applies a YQL schema whose table declares a vector index, through
// this provider: the index is created inside the table's CREATE TABLE, as a
// Go, YAML or HCL source's is, the next run is synced, a schema that leaves
// the index out drops it, since a YQL CREATE TABLE states every index of its
// table, and the run after that is synced, on both release lines. The
// provider refused the schema when it read it (G239); the built-in YDB
// engine at host 74de0b65b applied it.
func TestHostAppliesAYQLVectorIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _ := docsFixture(c)
	table := "CREATE TABLE `{dir}/docs` (id Int64 NOT NULL, gen Int64, emb String, body Utf8, PRIMARY KEY (id)%s);"
	indexed := fmt.Sprintf(table, ", INDEX docs_emb GLOBAL USING vector_kmeans_tree ON (gen, emb) COVER (body) "+
		"WITH (distance = cosine, vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2)")
	plain := fmt.Sprintf(table, "")

	applyYQLIndexRuns(c, directory, []yqlIndexRun{
		{name: "the creation", text: indexed, holds: "    INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) " +
			"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2)"},
		{name: "a second run", text: indexed, holds: "Schema is synced, no changes to be made"},
		{name: "the index left out", text: plain, holds: "Schema apply completed successfully", want: []string{"ALTER TABLE `docs` DROP INDEX `docs_emb`"}},
		{name: "a run without the index", text: plain, holds: "Schema is synced, no changes to be made"},
	})
}

// The same for a full-text index, on a line with full-text indexes (26.2).
func TestHostAppliesAYQLFullTextIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireSessionCapability(c, session, tablecreate.FullTextIndexesCapability)
	_, directory, _ := docsFixture(c)
	table := "CREATE TABLE `{dir}/docs` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id)%s);"
	indexed := fmt.Sprintf(table, ", INDEX ft GLOBAL USING fulltext_plain ON (body) WITH (tokenizer = 'standard', use_filter_lowercase = true)")
	plain := fmt.Sprintf(table, "")

	applyYQLIndexRuns(c, directory, []yqlIndexRun{
		{name: "the creation", text: indexed, holds: "    INDEX `ft` GLOBAL USING fulltext_plain ON (`body`) WITH (tokenizer=standard, use_filter_lowercase=true)"},
		{name: "a second run", text: indexed, holds: "Schema is synced, no changes to be made"},
		{name: "the index left out", text: plain, holds: "Schema apply completed successfully", want: []string{"ALTER TABLE `docs` DROP INDEX `ft`"}},
		{name: "a run without the index", text: plain, holds: "Schema is synced, no changes to be made"},
	})
}

// The same for a local index of a column table, on a line with local indexes
// (26.2): the column table's storage holds it, and a schema that leaves it
// out drops it.
func TestHostAppliesAYQLLocalIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireSessionCapability(c, session, tablecreate.LocalBloomIndexesCapability)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	table := "CREATE TABLE `{dir}/events` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id)%s) PARTITION BY HASH (id) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1);"
	indexed := fmt.Sprintf(table, ", INDEX bf LOCAL USING bloom_filter ON (body) WITH (false_positive_probability = 0.01)")
	plain := fmt.Sprintf(table, "")

	applyYQLIndexRuns(c, directory, []yqlIndexRun{
		{name: "the creation", text: indexed, holds: "    INDEX `bf` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.01)"},
		{name: "a second run", text: indexed, holds: "Schema is synced, no changes to be made"},
		{name: "the index left out", text: plain, holds: "Schema apply completed successfully", want: []string{"ALTER TABLE `events` DROP INDEX `bf`"}},
		{name: "a run without the index", text: plain, holds: "Schema is synced, no changes to be made"},
	})
}
