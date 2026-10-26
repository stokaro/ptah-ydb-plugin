package featureflags_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
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
	return map[string]bool{"transactional_ddl": false, "tiered_ttl": false, "external_data_sources": false, "external_object_replace": false, "resource_pools": false}
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
// changefeed topic is off on 25.1 and on from 25.2.
// Resource pools default to off on every line.
func linePreset(autoPartitionedFeeds bool) map[string]bool {
	caps := preset()
	caps["changefeed_topic_auto_partitioning"] = autoPartitionedFeeds
	caps["wide_date_time_types"] = autoPartitionedFeeds
	caps["parameterized_decimal"] = autoPartitionedFeeds
	caps["resource_pools"] = false
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
		{page: "local-ydb-25.1.4.7.json", preset: linePreset(false)},
		{page: "local-ydb-26.2.1.14.json", preset: linePreset(true)},
	} {
		t.Run(test.page, func(t *testing.T) {
			c := qt.New(t)
			flags, err := featureflags.Decode(page(c, test.page), "/local")
			c.Assert(err, qt.IsNil)
			c.Assert(flags.Refine(test.preset), qt.DeepEquals, test.preset)
		})
	}
}

// TestRefine_ClusterThatTurnedTieringOn reads a page from 26.2.1.14 whose
// dynamic configuration turned EnableTieringInColumnShard on.
func TestRefine_ClusterThatTurnedTieringOn(t *testing.T) {
	c := qt.New(t)
	flags, err := featureflags.Decode(page(c, "local-ydb-26.2.1.14-tiering-on.json"), "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(flags["EnableTieringInColumnShard"], qt.IsTrue)
	c.Assert(flags.Refine(linePreset(true)), qt.DeepEquals, with(map[string]bool{"tiered_ttl": true, "changefeed_topic_auto_partitioning": true, "external_data_sources": true, "wide_date_time_types": true, "parameterized_decimal": true}))
}

// TestRefine_HappyPath pins what each flag does to its capability, and that a
// flag the page does not list keeps the preset.
func TestRefine_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags featureflags.Flags
		want  map[string]bool
	}{
		{name: "wide time types on", flags: featureflags.Flags{"EnableTableDatetime64": true}, want: with(map[string]bool{"wide_date_time_types": true})},
		{name: "parameterized decimal on", flags: featureflags.Flags{"EnableParameterizedDecimal": true}, want: with(map[string]bool{"parameterized_decimal": true})},
		{name: "tiering on", flags: featureflags.Flags{"EnableTieringInColumnShard": true}, want: with(map[string]bool{"tiered_ttl": true})},
		{name: "tiering off", flags: featureflags.Flags{"EnableTieringInColumnShard": false}, want: preset()},
		{name: "tiering not listed", flags: featureflags.Flags{"EnableVectorIndex": true}, want: preset()},
		{name: "a flag of a capability the host owns", flags: featureflags.Flags{"EnableMoveIndex": true}, want: preset()},
		{name: "external data sources on", flags: featureflags.Flags{"EnableExternalDataSources": true}, want: with(map[string]bool{"external_data_sources": true})},
		{name: "external objects replaced in place", flags: featureflags.Flags{"EnableReplaceIfExistsForExternalEntities": true}, want: with(map[string]bool{"external_object_replace": true})},
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
			c := qt.New(t)
			c.Assert(test.flags.Refine(preset()), qt.DeepEquals, test.want)
		})
	}
}

// TestRefine_LeavesItsInputAlone returns a new set.
func TestRefine_LeavesItsInputAlone(t *testing.T) {
	c := qt.New(t)
	caps := preset()
	refined := featureflags.Flags{"EnableTieringInColumnShard": true}.Refine(caps)
	c.Assert(refined["tiered_ttl"], qt.IsTrue)
	c.Assert(caps, qt.DeepEquals, preset())
}

// TestRefine_EmptySet starts from no preset.
func TestRefine_EmptySet(t *testing.T) {
	c := qt.New(t)
	c.Assert(featureflags.Flags{"EnableTieringInColumnShard": true}.Refine(nil), qt.DeepEquals, map[string]bool{"tiered_ttl": true})
}

// TestGates_NameEveryFlagOnce keeps one flag per capability and one capability
// per flag.
func TestGates_NameEveryFlagOnce(t *testing.T) {
	c := qt.New(t)
	keys, flags := map[string]bool{}, map[string]bool{}
	for _, gate := range featureflags.Gates() {
		c.Assert(keys[gate.Key], qt.IsFalse, qt.Commentf(gate.Key))
		c.Assert(flags[gate.Flag], qt.IsFalse, qt.Commentf(gate.Flag))
		keys[gate.Key], flags[gate.Flag] = true, true
	}
	c.Assert(keys, qt.DeepEquals, map[string]bool{"tiered_ttl": true, "changefeed_topic_auto_partitioning": true, "external_data_sources": true, "external_object_replace": true, "resource_pools": true, "wide_date_time_types": true, "parameterized_decimal": true})
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
			c := qt.New(t)
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
			c := qt.New(t)
			flags, err := featureflags.Decode([]byte(test.body), "/local")
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(flags, qt.IsNil)
		})
	}
}
