// Package featureflags reads the feature flags a YDB database runs with from
// the monitoring endpoint's page and turns the flags that gate a capability
// the provider owns into that capability's value in the session.
//
// A release line runs with default flags, and an operator can turn a flag on
// or off, so one cluster can do more or less than its line's defaults say. The
// cluster's own page is the answer: `GET /viewer/json/feature_flags?database=<db>`
// lists every flag the database knows with its default and, where it was set,
// its current value. The page layout and the gate of each capability are the
// host's embedded engine's (internal/ydbflags in Ptah).
package featureflags

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Path is the monitoring page the flags are read from.
const Path = "/viewer/json/feature_flags"

// Flags is the value of every flag one database lists: the current value where
// the cluster set one, and the default otherwise.
type Flags map[string]bool

// Gate is one session capability a YDB feature flag decides.
type Gate struct {
	// Key is the capability, spelled as the host's vocabulary spells it.
	Key string
	// Flag is the flag's name as the monitoring page spells it.
	Flag string
}

// gates are the flags that decide a capability the provider plans with.
//
// tiered_ttl follows EnableTieringInColumnShard. The flag is off by default on
// every line from 25.1.4.7 to 26.2.1.14, and with it off a column table's TTL
// tier that moves rows to an external data source is refused with `Tiering
// functionality is disabled for OLAP tables` (measured on 26.2.1.14; turned on
// through the dynamic configuration, the same CREATE TABLE succeeds).
//
// The host maps more flags than these. The provider reports only the
// capabilities it owns, because the host plans the others itself: an
// index_rename the host read as true would make the host plan a RENAME INDEX
// that the provider's alteration does not carry out.
//
// external_data_sources follows EnableExternalDataSources and
// external_object_replace EnableReplaceIfExistsForExternalEntities, as the
// host maps them: both are off by default on every line, the first refuses
// every external data source and external table (`External data sources are
// disabled`), and the second refuses CREATE OR REPLACE of either object.
var gates = []Gate{
	{Key: "secrets", Flag: "EnableSchemaSecrets"},
	// Off by default on every line that lists it, 25.3 and later; 25.1 and
	// 25.2 do not list it and refuse a unique index on an existing table with
	// `Adding a unique index to an existing table is disabled`, which keeps
	// the preset's false.
	{Key: "unique_index_on_existing_table", Flag: "EnableAddUniqueIndex"},
	{Key: "alter_column_default", Flag: "EnableSetDropDefaultValue"},
	{Key: "add_column_with_default", Flag: "EnableAddColumsWithDefaults"},
	{Key: "wide_date_time_types", Flag: "EnableTableDatetime64"},
	{Key: "parameterized_decimal", Flag: "EnableParameterizedDecimal"},
	{Key: "tiered_ttl", Flag: "EnableTieringInColumnShard"},
	// Off on 25.1 and on from 25.2. With the flag on, 25.1 takes a changefeed
	// with TOPIC_AUTO_PARTITIONING = 'ENABLED' and reads it back as an
	// auto-partitioned topic; with it off, YDB answers `Topic autopartitioning
	// for CDC is disabled`.
	{Key: "changefeed_topic_auto_partitioning", Flag: "EnableTopicAutopartitioningForCDC"},
	{Key: "external_data_sources", Flag: "EnableExternalDataSources"},
	{Key: "external_object_replace", Flag: "EnableReplaceIfExistsForExternalEntities"},
	// Off by default on every line from 25.1.4.7 to 26.2.1.14. With it off,
	// YDB answers `Resource pools are disabled` and `Resource pool classifiers
	// are disabled`, and .sys/resource_pools lists nothing.
	{Key: "resource_pools", Flag: "EnableResourcePools"},
	// Off by default on 25.1.4.7 and on on 26.2.1.14. With it off, a vector
	// index is refused with `Vector index support is disabled`.
	{Key: "vector_indexes", Flag: "EnableVectorIndex"},
	// On by default on 26.2.1.14, and absent from the 25.1.4.7 page, which
	// leaves the preset's answer. With it off, a full-text index is refused
	// with `Fulltext index support is disabled`.
	{Key: "full_text_indexes", Flag: "EnableFulltextIndex"},
	// Off on 25.1 and on from 25.2. With the flag on, 25.1 creates a
	// transfer, moves a topic's message into its table, describes it and
	// changes its lambda and batch settings in place.
	{Key: "transfers", Flag: "EnableTopicTransfer"},
	// The local indexes of a column table, listed on 26.2 only: bloom_filter
	// and bloom_ngram_filter on by default, min_max off. With EnableLocalMinMaxIndex
	// off, 26.2.1.14 answers `Local min_max index is disabled with
	// EnableLocalMinMaxIndex feature flag`.
	{Key: "local_bloom_indexes", Flag: "EnableLocalBloomFilterIndex"},
	{Key: "local_ngram_indexes", Flag: "EnableLocalBloomNgramFilterIndex"},
	{Key: "local_min_max_indexes", Flag: "EnableLocalMinMaxIndex"},
	// Listed on 26.2 only, on by default; the lines before it create a
	// replication with no flag at all, so an absent flag keeps their preset.
	// Turned off, CREATE ASYNC REPLICATION answers `Asynchronous replication
	// is disabled`.
	{Key: "async_replication", Flag: "EnableReplication"},
}

// Gates returns every capability a flag decides, in a fixed order.
func Gates() []Gate { return slices.Clone(gates) }

// Refine returns caps with every gated capability set from its flag, where the
// flags list it. A flag the database does not list leaves its capability as
// caps has it. caps is not changed.
func (f Flags) Refine(caps map[string]bool) map[string]bool {
	refined := maps.Clone(caps)
	if refined == nil {
		refined = make(map[string]bool, len(gates))
	}
	for _, gate := range gates {
		if value, listed := f[gate.Flag]; listed {
			refined[gate.Key] = value
		}
	}
	if _, owned := refined["streaming_queries"]; owned {
		refined["streaming_queries"] = f["EnableStreamingQueries"] && f["EnableExternalDataSources"]
	}
	return refined
}

// pageVersion is the only page layout the provider reads, the one local-ydb
// serves from 25.1.4.7 to 26.2.1.14.
const pageVersion = 2

// page is the monitoring endpoint's answer.
type page struct {
	Version   int `json:"Version"`
	Databases []struct {
		Name         string `json:"Name"`
		FeatureFlags []struct {
			Name    string `json:"Name"`
			Default *bool  `json:"Default"`
			Current *bool  `json:"Current"`
		} `json:"FeatureFlags"`
	} `json:"Databases"`
}

// Decode reads the flags of database out of a feature-flags page. A page in
// another layout, one that does not list the database, a flag listed twice and
// a flag with no value are refused rather than read as flags that are off.
func Decode(body []byte, database string) (Flags, error) {
	var decoded page
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("the feature-flags page is not the JSON the provider reads")
	}
	if decoded.Version != pageVersion {
		return nil, fmt.Errorf("the feature-flags page has version %d, and the provider reads version %d", decoded.Version, pageVersion)
	}
	want := "/" + strings.Trim(database, "/")
	for _, listed := range decoded.Databases {
		if "/"+strings.Trim(listed.Name, "/") != want {
			continue
		}
		flags := make(Flags, len(listed.FeatureFlags))
		for _, flag := range listed.FeatureFlags {
			if _, repeated := flags[flag.Name]; repeated {
				return nil, fmt.Errorf("the feature-flags page lists %s twice", flag.Name)
			}
			switch {
			case flag.Current != nil:
				flags[flag.Name] = *flag.Current
			case flag.Default != nil:
				flags[flag.Name] = *flag.Default
			default:
				return nil, fmt.Errorf("the feature-flags page lists %s with no value", flag.Name)
			}
		}
		return flags, nil
	}
	return nil, fmt.Errorf("the feature-flags page does not list database %s", want)
}
