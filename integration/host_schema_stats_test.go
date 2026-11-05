//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host's built-in YDB engine counted the settings of a directory's tables
// in seven families of `schema stats`. For a host that negotiates
// models.report-counts, the provider counts them the same way: row tables with
// a TTL, and a column table whose TTL only deletes rows; the column families
// a table states, without a default family that states nothing; row tables
// whose partitioning, read replicas or key bloom filter differ from what YDB
// gives a new table; column tables, and the one whose TTL moves rows to an
// external data source; and vector indexes. Each value is the one the built-in
// engine printed for the same directory, on 25.1.4.7 and 26.2.1.14 alike.
//
// A global index is counted once where its partitioning or read replicas
// differ from the ones YDB gives a new index: four here, one of them
// asynchronous and one unique, and neither the index that keeps YDB's
// settings nor the vector indexes. The host reads a global index as a common
// index, so the count comes from the index's own settings, which a host reads
// when it negotiates schema.observed-index-facets.
func TestHostSchemaStatsCountsTheTableSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ReportCountsFeature)
	requireHostNegotiates(c, provider.ObservedIndexFacetsFeature)
	f := newMigrationsFixture(c)
	absolute := func(name string) string { return path.Join(f.database, f.dir, name) }
	execAll(c, f,
		"CREATE TABLE `"+absolute("plain")+"` (id Uint64 NOT NULL, name Utf8, PRIMARY KEY (id), INDEX plain_name GLOBAL ON (name))",
		"CREATE TABLE `"+absolute("tuned")+"` (id Uint64 NOT NULL, a Utf8, b Utf8, d Utf8, e Utf8, PRIMARY KEY (id), "+
			"INDEX by_load GLOBAL ON (a), INDEX by_replicas GLOBAL ASYNC ON (b), INDEX by_minimum GLOBAL ON (d), INDEX untouched GLOBAL ON (e))",
		"ALTER TABLE `"+absolute("tuned")+"` ALTER INDEX by_load SET (AUTO_PARTITIONING_BY_LOAD = ENABLED)",
		"ALTER TABLE `"+absolute("tuned")+"` ALTER INDEX by_replicas SET (READ_REPLICAS_SETTINGS = 'PER_AZ:1')",
		"ALTER TABLE `"+absolute("tuned")+"` ALTER INDEX by_minimum SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)",
		"CREATE TABLE `"+absolute("unique")+"` (id Uint64 NOT NULL, u Utf8, PRIMARY KEY (id), INDEX by_u GLOBAL UNIQUE SYNC ON (u))",
		"ALTER TABLE `"+absolute("unique")+"` ALTER INDEX by_u SET (AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9)",
		"CREATE TABLE `"+absolute("ttl_date")+"` (id Uint64 NOT NULL, ts Timestamp, PRIMARY KEY (id)) WITH (TTL = Interval(\"P1D\") ON ts)",
		"CREATE TABLE `"+absolute("ttl_epoch")+"` (id Uint64 NOT NULL, expiry Uint64, PRIMARY KEY (id)) WITH (TTL = Interval(\"PT1H\") ON expiry AS SECONDS)",
		"CREATE TABLE `"+absolute("families")+"` (id Uint64 NOT NULL, a Utf8 FAMILY cold, b Utf8, PRIMARY KEY (id), "+
			"FAMILY default (COMPRESSION = \"lz4\"), FAMILY cold (COMPRESSION = \"off\"))",
		"CREATE TABLE `"+absolute("one_family")+"` (id Uint64 NOT NULL, a Utf8 FAMILY cold, PRIMARY KEY (id), FAMILY cold (COMPRESSION = \"lz4\"))",
		"CREATE TABLE `"+absolute("by_load")+"` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED)",
		"CREATE TABLE `"+absolute("bloom")+"` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (KEY_BLOOM_FILTER = ENABLED)",
		"CREATE TABLE `"+absolute("replicas")+"` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (READ_REPLICAS_SETTINGS = \"PER_AZ:1\")",
		"CREATE TABLE `"+absolute("bounded")+"` (id Uint64 NOT NULL, PRIMARY KEY (id)) "+
			"WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9)",
		// A starting layout leaves the minimum it sets, which differs from YDB's.
		"CREATE TABLE `"+absolute("uniform")+"` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (UNIFORM_PARTITIONS = 4)",
		"CREATE TABLE `"+absolute("vectors")+"` (id Uint64 NOT NULL, emb String, PRIMARY KEY (id))",
		"ALTER TABLE `"+absolute("vectors")+"` ADD INDEX by_cosine GLOBAL USING vector_kmeans_tree ON (emb) "+
			"WITH (distance=cosine, vector_type=float, vector_dimension=2, levels=1, clusters=2)",
		"ALTER TABLE `"+absolute("vectors")+"` ADD INDEX by_product GLOBAL USING vector_kmeans_tree ON (emb) "+
			"WITH (similarity=inner_product, vector_type=float, vector_dimension=2, levels=1, clusters=2)",
		"CREATE TABLE `"+absolute("columns")+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id)) PARTITION BY HASH(id) WITH (STORE = COLUMN)",
		"CREATE TABLE `"+absolute("columns_ttl")+"` (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) PARTITION BY HASH(id) "+
			"WITH (STORE = COLUMN, TTL = Interval(\"P1D\") ON ts)",
	)
	// A TTL that moves rows needs tiering on and a data source to move them to.
	newClusterFlags(c, f.opened).set(c, map[string]bool{"EnableTieringInColumnShard": true, "EnableExternalDataSources": true})
	suffix := strings.ToLower(rand.Text())
	access, secret := "ptah_stats_access_"+suffix, "ptah_stats_secret_"+suffix
	execAll(c, f,
		"CREATE OBJECT "+access+" (TYPE SECRET) WITH (value='fixture-access')",
		"CREATE OBJECT "+secret+" (TYPE SECRET) WITH (value='fixture-secret')",
		"CREATE EXTERNAL DATA SOURCE `"+absolute("archive")+"` WITH (SOURCE_TYPE='ObjectStorage', LOCATION='https://column.invalid/archive/', "+
			"AUTH_METHOD='AWS', AWS_REGION='us-east-1', AWS_ACCESS_KEY_ID_SECRET_NAME='"+access+"', AWS_SECRET_ACCESS_KEY_SECRET_NAME='"+secret+"')",
		"CREATE TABLE `"+absolute("columns_tiered")+"` (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) PARTITION BY HASH(id) "+
			"WITH (STORE = COLUMN, TTL = Interval(\"P1D\") TO EXTERNAL DATA SOURCE `"+absolute("archive")+"`, Interval(\"P7D\") DELETE ON ts)",
	)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The table goes before the data source its TTL names.
		for _, statement := range []string{"DROP TABLE `" + absolute("columns_tiered") + "`", "DROP EXTERNAL DATA SOURCE `" + absolute("archive") + "`",
			"DROP OBJECT " + access + " (TYPE SECRET)", "DROP OBJECT " + secret + " (TYPE SECRET)"} {
			c.Check(execFixtureSchema(cleanup, f.opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})

	stdout, err := hostCommand(c, "schema", "stats", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
	labels := `{dialect="ydb",schemas="` + f.dir + `"}`
	for _, sample := range []string{
		"ptah_schema_tables" + labels + " 16\n",
		"ptah_schema_ydb_ttl_tables" + labels + " 3\n",
		"ptah_schema_ydb_column_families" + labels + " 3\n",
		"ptah_schema_ydb_table_partitioning" + labels + " 5\n",
		"ptah_schema_ydb_index_partitioning" + labels + " 4\n",
		"ptah_schema_ydb_column_tables" + labels + " 3\n",
		"ptah_schema_ydb_tiered_ttl_tables" + labels + " 1\n",
		"ptah_schema_ydb_vector_indexes" + labels + " 2\n",
		"# HELP ptah_schema_ydb_ttl_tables Tables with a captured YDB TTL.\n",
		"# HELP ptah_schema_ydb_column_families YDB column families a table states, without a default family that states nothing.\n",
		"# HELP ptah_schema_ydb_table_partitioning YDB row tables stating partitioning, read replicas or a key bloom filter.\n",
		"# HELP ptah_schema_ydb_index_partitioning YDB global indexes stating partitioning or read replicas.\n",
		"# HELP ptah_schema_ydb_column_tables YDB tables stored by column.\n",
		"# HELP ptah_schema_ydb_tiered_ttl_tables YDB column tables whose TTL moves rows to an external data source.\n",
		"# HELP ptah_schema_ydb_vector_indexes YDB vector indexes with captured settings.\n",
	} {
		c.Assert(stdout, qt.Contains, sample)
	}
}

// The built-in engine counted every index of a directory among
// ptah_schema_indexes: a global index, a vector index, a full-text index and
// a column table's local index alike. Through the provider the host counts
// the global indexes it reads as common indexes, and the provider counts the
// others, which its own models hold, under the host's metric of indexes. Each
// value is the one the built-in engine at host 74de0b65b printed for the same
// directory, on 25.1.4.7 and 26.2.1.14; 25.1 has neither full-text nor local
// indexes.
func TestHostSchemaStatsCountsTheIndexesTheProviderHolds_HappyPath(t *testing.T) {
	capabilities := targetCapabilities(testcheck.New(t))
	for _, test := range []struct {
		name, capability string
		statements       []string
		want             string
	}{
		{name: "a global index and two vector indexes", statements: []string{
			"CREATE TABLE `<dir>/plain` (id Uint64 NOT NULL, name Utf8, PRIMARY KEY (id), INDEX plain_name GLOBAL ON (name))",
			"CREATE TABLE `<dir>/vectors` (id Uint64 NOT NULL, emb String, PRIMARY KEY (id))",
			"ALTER TABLE `<dir>/vectors` ADD INDEX by_cosine GLOBAL USING vector_kmeans_tree ON (emb) " +
				"WITH (distance=cosine, vector_type=float, vector_dimension=2, levels=1, clusters=2)",
			"ALTER TABLE `<dir>/vectors` ADD INDEX by_product GLOBAL USING vector_kmeans_tree ON (emb) " +
				"WITH (similarity=inner_product, vector_type=float, vector_dimension=2, levels=1, clusters=2)",
		}, want: "3"},
		{name: "a full-text index", capability: "full_text_indexes", statements: []string{
			"CREATE TABLE `<dir>/texts` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id), INDEX ft GLOBAL USING fulltext_plain ON (body) WITH (tokenizer=standard))",
		}, want: "1"},
		{name: "a column table's local index", capability: "local_bloom_indexes", statements: []string{
			"CREATE TABLE `<dir>/columns` (id Uint64 NOT NULL, ts Timestamp NOT NULL, body Utf8, PRIMARY KEY (ts, id), " +
				"INDEX bf LOCAL USING bloom_filter ON (body)) PARTITION BY HASH (id) WITH (STORE = COLUMN)",
		}, want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCapability(c, capabilities, test.capability)
			requireHostNegotiates(c, provider.ReportCountsFeature)
			f := newMigrationsFixture(c)
			for _, statement := range test.statements {
				execAll(c, f, strings.ReplaceAll(statement, "<dir>", path.Join(f.database, f.dir)))
			}

			stdout, err := hostCommand(c, "schema", "stats", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", f.dir)

			c.Assert(err, qt.IsNil, qt.Commentf("%s", stdout))
			c.Assert(stdout, qt.Contains, "ptah_schema_indexes{dialect=\"ydb\",schemas=\""+f.dir+"\"} "+test.want+"\n")
			c.Assert(strings.Count(stdout, "# TYPE ptah_schema_indexes "), qt.Equals, 1)
		})
	}
}
