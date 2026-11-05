package featureflags_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// page reads one feature-flags page the monitoring endpoint of a local-ydb
// container answered, unedited. testdata/README.md says where each came from.
func page(c *qt.C, name string) []byte {
	c.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	c.Assert(err, qt.IsNil)
	return body
}

// preset is the session's capability set before the flags are read.
func preset() map[string]bool {
	return map[string]bool{"transactional_ddl": false, "secrets": false, "tiered_ttl": false, "external_data_sources": false, "external_object_replace": false, "resource_pools": false}
}

// with is the preset with some capabilities set.
func with(set map[string]bool) map[string]bool {
	caps := preset()
	for key, value := range set {
		caps[key] = value
	}
	return caps
}

// linePreset is the preset of one release line: an auto-partitioned
// changefeed topic, a vector index and a transfer are off on 25.1 and on from
// 25.2, and a full-text index and the bloom_filter and bloom_ngram_filter
// local indexes are on from 26.2. Resource pools and min_max local indexes
// default to off on every line. 26.2 lists EnableReplication, on by default,
// and 25.1 does not list it. EnableMoveIndex is on by default on both, so an
// index is renamed in place. A family cache mode is on with 26.2's default
// EnableTableCacheModes, and 25.1 does not list the flag.
func linePreset(autoPartitionedFeeds bool, secrets bool) map[string]bool {
	caps := preset()
	caps["changefeed_topic_auto_partitioning"] = autoPartitionedFeeds
	caps["wide_date_time_types"] = autoPartitionedFeeds
	caps["parameterized_decimal"] = autoPartitionedFeeds
	caps["resource_pools"] = false
	caps["secrets"] = secrets
	caps["add_column_with_default"] = secrets
	caps["vector_indexes"] = autoPartitionedFeeds
	caps["full_text_indexes"] = secrets
	caps["local_bloom_indexes"] = secrets
	caps["local_ngram_indexes"] = secrets
	caps["local_min_max_indexes"] = false
	caps["transfers"] = autoPartitionedFeeds
	caps["unique_index_on_existing_table"] = false
	caps["index_rename"] = true
	caps["column_family_cache_mode"] = secrets
	if autoPartitionedFeeds {
		caps["alter_column_default"] = true
		caps["async_replication"] = true
	}
	return caps
}

// TestRefine_EveryLineDefaultIsThePreset is the measurement the mapping rests
// on: a line's flags at their defaults leave the session's preset as it is. A
// gate whose flag defaulted otherwise would turn a cluster nobody configured
// into one the provider plans for differently.
func TestRefine_EveryLineDefaultIsThePreset(t *testing.T) {
	for _, test := range []struct {
		page   string
		preset map[string]bool
	}{
		{page: "local-ydb-25.1.4.7.json", preset: linePreset(false, false)},
		{page: "local-ydb-26.2.1.14.json", preset: linePreset(true, true)},
	} {
		t.Run(test.page, func(t *testing.T) {
			c := testcheck.New(t)
			flags, err := featureflags.Decode(page(c, test.page), "/local")
			c.Assert(err, qt.IsNil)
			c.Assert(flags.Refine(test.preset), qt.DeepEquals, test.preset)
		})
	}
}

// TestRefine_ClusterThatTurnedTieringOn reads a page from 26.2.1.14 whose
// dynamic configuration turned EnableTieringInColumnShard on. The server ran
// with enable_add_unique_index, as the CI's 26.2 server does.
func TestRefine_ClusterThatTurnedTieringOn(t *testing.T) {
	c := testcheck.New(t)
	flags, err := featureflags.Decode(page(c, "local-ydb-26.2.1.14-tiering-on.json"), "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(flags["EnableTieringInColumnShard"], qt.IsTrue)
	c.Assert(flags.Refine(linePreset(true, true)), qt.DeepEquals, with(map[string]bool{"secrets": true, "alter_column_default": true, "add_column_with_default": true, "tiered_ttl": true, "changefeed_topic_auto_partitioning": true, "external_data_sources": true, "wide_date_time_types": true, "parameterized_decimal": true, "vector_indexes": true, "full_text_indexes": true, "transfers": true, "async_replication": true,
		"local_bloom_indexes": true, "local_ngram_indexes": true, "local_min_max_indexes": false, "unique_index_on_existing_table": true, "index_rename": true,
		"column_family_cache_mode": true}))
}

// TestRefine_HappyPath pins what each flag does to its capability, and that a
// flag the page does not list keeps the preset.
func TestRefine_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags featureflags.Flags
		want  map[string]bool
	}{
		{name: "default addition off", flags: featureflags.Flags{"EnableAddColumsWithDefaults": false}, want: with(map[string]bool{"add_column_with_default": false})},
		{name: "default addition on", flags: featureflags.Flags{"EnableAddColumsWithDefaults": true}, want: with(map[string]bool{"add_column_with_default": true})},
		{name: "default alteration off", flags: featureflags.Flags{"EnableSetDropDefaultValue": false}, want: with(map[string]bool{"alter_column_default": false})},
		{name: "default alteration on", flags: featureflags.Flags{"EnableSetDropDefaultValue": true}, want: with(map[string]bool{"alter_column_default": true})},
		{name: "wide time types on", flags: featureflags.Flags{"EnableTableDatetime64": true}, want: with(map[string]bool{"wide_date_time_types": true})},
		{name: "parameterized decimal on", flags: featureflags.Flags{"EnableParameterizedDecimal": true}, want: with(map[string]bool{"parameterized_decimal": true})},
		{name: "tiering on", flags: featureflags.Flags{"EnableTieringInColumnShard": true}, want: with(map[string]bool{"tiered_ttl": true})},
		{name: "tiering off", flags: featureflags.Flags{"EnableTieringInColumnShard": false}, want: preset()},
		{name: "tiering not listed", flags: featureflags.Flags{"EnableTempTables": true}, want: preset()},
		// 25.3 and later list EnableAddUniqueIndex, off by default.
		{name: "unique indexes on existing tables on", flags: featureflags.Flags{"EnableAddUniqueIndex": true}, want: with(map[string]bool{"unique_index_on_existing_table": true})},
		{name: "unique indexes on existing tables off", flags: featureflags.Flags{"EnableAddUniqueIndex": false}, want: with(map[string]bool{"unique_index_on_existing_table": false})},
		// 25.1 keeps a vector index behind EnableVectorIndex, and every line
		// a full-text one behind EnableFulltextIndex.
		{name: "vector indexes off", flags: featureflags.Flags{"EnableVectorIndex": false}, want: with(map[string]bool{"vector_indexes": false})},
		{name: "vector indexes on", flags: featureflags.Flags{"EnableVectorIndex": true}, want: with(map[string]bool{"vector_indexes": true})},
		{name: "full-text indexes off", flags: featureflags.Flags{"EnableFulltextIndex": false}, want: with(map[string]bool{"full_text_indexes": false})},
		{name: "full-text indexes on", flags: featureflags.Flags{"EnableFulltextIndex": true}, want: with(map[string]bool{"full_text_indexes": true})},
		// 25.3 lists EnableTableCacheModes, off by default, and 25.4 and
		// later on by default.
		{name: "family cache modes off", flags: featureflags.Flags{"EnableTableCacheModes": false}, want: with(map[string]bool{"column_family_cache_mode": false})},
		{name: "family cache modes on", flags: featureflags.Flags{"EnableTableCacheModes": true}, want: with(map[string]bool{"column_family_cache_mode": true})},
		{name: "index renames off", flags: featureflags.Flags{"EnableMoveIndex": false}, want: with(map[string]bool{"index_rename": false})},
		{name: "index renames on", flags: featureflags.Flags{"EnableMoveIndex": true}, want: with(map[string]bool{"index_rename": true})},
		// 26.2 lists the three local-index flags, and the lines before it
		// none of them.
		{name: "local bloom indexes off", flags: featureflags.Flags{"EnableLocalBloomFilterIndex": false}, want: with(map[string]bool{"local_bloom_indexes": false})},
		{name: "local bloom indexes on", flags: featureflags.Flags{"EnableLocalBloomFilterIndex": true}, want: with(map[string]bool{"local_bloom_indexes": true})},
		{name: "local n-gram indexes off", flags: featureflags.Flags{"EnableLocalBloomNgramFilterIndex": false}, want: with(map[string]bool{"local_ngram_indexes": false})},
		{name: "local n-gram indexes on", flags: featureflags.Flags{"EnableLocalBloomNgramFilterIndex": true}, want: with(map[string]bool{"local_ngram_indexes": true})},
		{name: "local min_max indexes off", flags: featureflags.Flags{"EnableLocalMinMaxIndex": false}, want: with(map[string]bool{"local_min_max_indexes": false})},
		{name: "local min_max indexes on", flags: featureflags.Flags{"EnableLocalMinMaxIndex": true}, want: with(map[string]bool{"local_min_max_indexes": true})},
		{name: "external data sources on", flags: featureflags.Flags{"EnableExternalDataSources": true}, want: with(map[string]bool{"external_data_sources": true})},
		{name: "external objects replaced in place", flags: featureflags.Flags{"EnableReplaceIfExistsForExternalEntities": true}, want: with(map[string]bool{"external_object_replace": true})},
		{name: "transfers on", flags: featureflags.Flags{"EnableTopicTransfer": true}, want: with(map[string]bool{"transfers": true})},
		{name: "transfers off", flags: featureflags.Flags{"EnableTopicTransfer": false}, want: with(map[string]bool{"transfers": false})},
		{name: "async replication off", flags: featureflags.Flags{"EnableReplication": false}, want: with(map[string]bool{"async_replication": false})},
		// A 25.1 cluster with the flag on takes an auto-partitioned changefeed
		// topic, and a later one with it off refuses one.
		{name: "auto-partitioned changefeed topics on", flags: featureflags.Flags{"EnableTopicAutopartitioningForCDC": true},
			want: with(map[string]bool{"changefeed_topic_auto_partitioning": true})},
		{name: "auto-partitioned changefeed topics off", flags: featureflags.Flags{"EnableTopicAutopartitioningForCDC": false},
			want: with(map[string]bool{"changefeed_topic_auto_partitioning": false})},
		{name: "resource pools on", flags: featureflags.Flags{"EnableResourcePools": true},
			want: with(map[string]bool{"resource_pools": true})},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.flags.Refine(preset()), qt.DeepEquals, test.want)
		})
	}
}

// TestRefine_LeavesItsInputAlone returns a new set.
func TestRefine_LeavesItsInputAlone(t *testing.T) {
	c := testcheck.New(t)
	caps := preset()
	refined := featureflags.Flags{"EnableTieringInColumnShard": true}.Refine(caps)
	c.Assert(refined["tiered_ttl"], qt.IsTrue)
	c.Assert(caps, qt.DeepEquals, preset())
}

// TestRefine_EmptySet starts from no preset.
func TestRefine_EmptySet(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(featureflags.Flags{"EnableTieringInColumnShard": true}.Refine(nil), qt.DeepEquals, map[string]bool{"tiered_ttl": true})
}

// TestGates_NameEveryFlagOnce keeps one flag per capability and one capability
// per flag.
func TestGates_NameEveryFlagOnce(t *testing.T) {
	c := testcheck.New(t)
	keys, flags := map[string]bool{}, map[string]bool{}
	for _, gate := range featureflags.Gates() {
		c.Assert(keys[gate.Key], qt.IsFalse, qt.Commentf(gate.Key))
		c.Assert(flags[gate.Flag], qt.IsFalse, qt.Commentf(gate.Flag))
		keys[gate.Key], flags[gate.Flag] = true, true
	}
	c.Assert(keys, qt.DeepEquals, map[string]bool{"secrets": true, "alter_column_default": true, "add_column_with_default": true, "tiered_ttl": true, "changefeed_topic_auto_partitioning": true, "external_data_sources": true, "external_object_replace": true, "resource_pools": true, "wide_date_time_types": true, "parameterized_decimal": true, "vector_indexes": true, "full_text_indexes": true, "transfers": true, "async_replication": true,
		"local_bloom_indexes": true, "local_ngram_indexes": true, "local_min_max_indexes": true, "unique_index_on_existing_table": true, "index_rename": true,
		"column_family_cache_mode": true})
}

func TestDecode_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, body, database string
		want                 featureflags.Flags
	}{
		{name: "the current value wins", database: "/local",
			body: `{"Version":2,"Databases":[{"Name":"/local","FeatureFlags":[{"Name":"EnableTieringInColumnShard","Default":false,"Current":true}]}]}`,
			want: featureflags.Flags{"EnableTieringInColumnShard": true}},
		{name: "the default stands where nothing is set", database: "/local",
			body: `{"Version":2,"Databases":[{"Name":"/local","FeatureFlags":[{"Name":"EnableTieringInColumnShard","Default":false}]}]}`,
			want: featureflags.Flags{"EnableTieringInColumnShard": false}},
		{name: "the database is matched without its slashes", database: "local/",
			body: `{"Version":2,"Databases":[{"Name":"/other","FeatureFlags":[{"Name":"A","Default":true}]},{"Name":"/local/","FeatureFlags":[{"Name":"B","Default":true}]}]}`,
			want: featureflags.Flags{"B": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			flags, err := featureflags.Decode([]byte(test.body), test.database)
			c.Assert(err, qt.IsNil)
			c.Assert(flags, qt.DeepEquals, test.want)
		})
	}
}

func TestDecode_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, body, wantErr string
	}{
		{name: "not JSON", body: "Failed to resolve database", wantErr: `the feature-flags page is not the JSON the provider reads`},
		{name: "another page layout", body: `{"Version":3,"Databases":[]}`, wantErr: `the feature-flags page has version 3, and the provider reads version 2`},
		{name: "the database is not listed", body: `{"Version":2,"Databases":[{"Name":"/other","FeatureFlags":[]}]}`,
			wantErr: `the feature-flags page does not list database /local`},
		{name: "a flag with no value", body: `{"Version":2,"Databases":[{"Name":"/local","FeatureFlags":[{"Name":"EnableTieringInColumnShard"}]}]}`,
			wantErr: `the feature-flags page lists EnableTieringInColumnShard with no value`},
		{name: "a flag listed twice", body: `{"Version":2,"Databases":[{"Name":"/local","FeatureFlags":[{"Name":"A","Default":true},{"Name":"A","Default":false}]}]}`,
			wantErr: `the feature-flags page lists A twice`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			flags, err := featureflags.Decode([]byte(test.body), "/local")
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(flags, qt.IsNil)
		})
	}
}

// TestClause_HappyPath pins what a refusal adds for a capability a flag
// decides, and that a key no flag decides adds nothing.
func TestClause_HappyPath(t *testing.T) {
	for _, test := range []struct{ key, want string }{
		{"unique_index_on_existing_table", "; YDB keeps it behind feature flag EnableAddUniqueIndex: turn the flag on, and name the cluster's monitoring endpoint " +
			"in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"},
		{"external_data_sources", "; YDB keeps it behind feature flag EnableExternalDataSources: turn the flag on, and name the cluster's monitoring endpoint " +
			"in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"},
		{"topics", ""},
		{"external_data_source_secret_paths", ""},
	} {
		t.Run(test.key, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(featureflags.Clause(test.key), qt.Equals, test.want)
		})
	}
}

// Each refusal text the host's embedded engine measured names its gate: the
// capability and the flag YDB keeps it behind. Streaming queries need two
// flags, and YDB's refusal names the one it was refused for.
func TestRefused_HappyPath(t *testing.T) {
	for _, test := range []struct {
		issue, key, flag string
	}{
		{"Secrets are disabled", "secrets", "EnableSchemaSecrets"},
		{"Adding a unique index to an existing table is disabled", "unique_index_on_existing_table", "EnableAddUniqueIndex"},
		{"Move index is not supported yet", "index_rename", "EnableMoveIndex"},
		{"Set/drop default value is not enabled", "alter_column_default", "EnableSetDropDefaultValue"},
		{"Column addition with default value is not supported now", "add_column_with_default", "EnableAddColumsWithDefaults"},
		{"EnableTableDatetime64 feature flag is off", "wide_date_time_types", "EnableTableDatetime64"},
		{"Tiering functionality is disabled for OLAP tables", "tiered_ttl", "EnableTieringInColumnShard"},
		{"Resource pool classifiers are disabled", "resource_pools", "EnableResourcePools"},
		{"Local min_max index is disabled with EnableLocalMinMaxIndex feature flag", "local_min_max_indexes", "EnableLocalMinMaxIndex"},
		{"Asynchronous replication is disabled", "async_replication", "EnableReplication"},
		{"Setting cache_mode is not allowed", "column_family_cache_mode", "EnableTableCacheModes"},
		{"Streaming queries are disabled", "streaming_queries", "EnableStreamingQueries"},
	} {
		t.Run(test.issue, func(t *testing.T) {
			c := testcheck.New(t)
			gate, off := featureflags.Refused([]string{"Execution failed", test.issue})
			c.Assert(off, qt.IsTrue)
			c.Assert(gate.Key, qt.Equals, test.key)
			c.Assert(gate.Flag, qt.Equals, test.flag)
		})
	}
}

// A refusal for another reason names no gate.
func TestRefused_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	gate, off := featureflags.Refused([]string{"Cannot find table 'db.[/local/t]' because it does not exist or you do not have access permissions."})
	c.Assert(off, qt.IsFalse)
	c.Assert(gate.Key+gate.Flag, qt.Equals, "")
}
