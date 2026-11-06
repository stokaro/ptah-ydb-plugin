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
	// refusals are texts YDB's refusal holds when the flag is off, each
	// measured by the host's embedded engine (internal/ydbflags in Ptah); a
	// gate whose refusal was never seen has none.
	refusals []string
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
// capabilities it owns, because the host plans the others itself.
//
// index_rename follows EnableMoveIndex, on by default on every line from
// 25.1.4.7 to 26.2.1.14. With it off, YDB refuses a rename with `Move index is
// not supported yet` and the index keeps its name.
//
// external_data_sources follows EnableExternalDataSources and
// external_object_replace EnableReplaceIfExistsForExternalEntities, as the
// host maps them: both are off by default on every line, the first refuses
// every external data source and external table (`External data sources are
// disabled`), and the second refuses CREATE OR REPLACE of either object.
var gates = []Gate{
	{Key: "secrets", Flag: "EnableSchemaSecrets", refusals: []string{"Secrets are disabled"}},
	// Off by default on every line that lists it, 25.3 and later; 25.1 and
	// 25.2 do not list it and refuse a unique index on an existing table with
	// `Adding a unique index to an existing table is disabled`, which keeps
	// the preset's false.
	{Key: "unique_index_on_existing_table", Flag: "EnableAddUniqueIndex", refusals: []string{"Adding a unique index to an existing table is disabled"}},
	{Key: "index_rename", Flag: "EnableMoveIndex", refusals: []string{"Move index is not supported yet"}},
	{Key: "alter_column_default", Flag: "EnableSetDropDefaultValue", refusals: []string{"Set/drop default value is not enabled"}},
	{Key: "add_column_with_default", Flag: "EnableAddColumsWithDefaults",
		refusals: []string{"Adding columns with defaults is disabled", "Column addition with default value is not supported now"}},
	{Key: "wide_date_time_types", Flag: "EnableTableDatetime64", refusals: []string{"EnableTableDatetime64 feature flag is off"}},
	{Key: "parameterized_decimal", Flag: "EnableParameterizedDecimal", refusals: []string{"EnableParameterizedDecimal feature flag is off"}},
	{Key: "tiered_ttl", Flag: "EnableTieringInColumnShard", refusals: []string{"Tiering functionality is disabled for OLAP tables"}},
	// Off on 25.1 and on from 25.2. With the flag on, 25.1 takes a changefeed
	// with TOPIC_AUTO_PARTITIONING = 'ENABLED' and reads it back as an
	// auto-partitioned topic; with it off, YDB answers `Topic autopartitioning
	// for CDC is disabled`.
	{Key: "changefeed_topic_auto_partitioning", Flag: "EnableTopicAutopartitioningForCDC", refusals: []string{"Topic autopartitioning for CDC is disabled"}},
	{Key: "external_data_sources", Flag: "EnableExternalDataSources", refusals: []string{"External data sources are disabled"}},
	{Key: "external_object_replace", Flag: "EnableReplaceIfExistsForExternalEntities",
		refusals: []string{"feature flag EnableReplaceIfExistsForExternalEntities is off"}},
	// Off by default on every line from 25.1.4.7 to 26.2.1.14. With it off,
	// YDB answers `Resource pools are disabled` and `Resource pool classifiers
	// are disabled`, and .sys/resource_pools lists nothing.
	{Key: "resource_pools", Flag: "EnableResourcePools", refusals: []string{"Resource pools are disabled", "Resource pool classifiers are disabled"}},
	// Off by default on 25.1.4.7 and on on 26.2.1.14. With it off, a vector
	// index is refused with `Vector index support is disabled`.
	{Key: "vector_indexes", Flag: "EnableVectorIndex", refusals: []string{"Vector index support is disabled"}},
	// On by default on 26.2.1.14, and absent from the 25.1.4.7 page, which
	// leaves the preset's answer. With it off, a full-text index is refused
	// with `Fulltext index support is disabled`.
	{Key: "full_text_indexes", Flag: "EnableFulltextIndex", refusals: []string{"Fulltext index support is disabled"}},
	// Off on 25.1 and on from 25.2. With the flag on, 25.1 creates a
	// transfer, moves a topic's message into its table, describes it and
	// changes its lambda and batch settings in place.
	{Key: "transfers", Flag: "EnableTopicTransfer", refusals: []string{"Topic transfer creation is disabled"}},
	// The local indexes of a column table, listed on 26.2 only: bloom_filter
	// and bloom_ngram_filter on by default, min_max off. With EnableLocalMinMaxIndex
	// off, 26.2.1.14 answers `Local min_max index is disabled with
	// EnableLocalMinMaxIndex feature flag`.
	{Key: "local_bloom_indexes", Flag: "EnableLocalBloomFilterIndex", refusals: []string{"Local bloom filter index support is disabled"}},
	{Key: "local_ngram_indexes", Flag: "EnableLocalBloomNgramFilterIndex", refusals: []string{"Local bloom ngram filter index support is disabled"}},
	{Key: "local_min_max_indexes", Flag: "EnableLocalMinMaxIndex", refusals: []string{"Local min_max index is disabled"}},
	// Listed on 26.2 only, on by default; the lines before it create a
	// replication with no flag at all, so an absent flag keeps their preset.
	// Turned off, CREATE ASYNC REPLICATION answers `Asynchronous replication
	// is disabled`.
	{Key: "async_replication", Flag: "EnableReplication", refusals: []string{"Asynchronous replication is disabled"}},
	// A column family's cache mode: on by default on 26.2.1.14, absent from
	// the 25.1.4.7 page, which leaves the preset's false. With it off, YDB
	// answers `Setting cache_mode is not allowed`, as the host measured on
	// 25.3.
	{Key: "column_family_cache_mode", Flag: "EnableTableCacheModes", refusals: []string{"Setting cache_mode is not allowed"}},
}

// streaming is the gate of streaming_queries, which needs EnableStreamingQueries
// and EnableExternalDataSources together (see [Flags.Refine]), so it is not
// one of gates. A refusal still names the flag YDB refused it for.
var streaming = Gate{Key: "streaming_queries", Flag: "EnableStreamingQueries", refusals: []string{"Streaming queries are disabled"}}

// Gates returns every capability a flag decides, in a fixed order.
func Gates() []Gate { return slices.Clone(gates) }

// Refused returns the gate whose flag YDB's refusal says is off, and false
// when the refusal is about something else. issues are the messages of the
// issues YDB answered with; a refusal holds one of the gate's texts. The
// texts and their gates are the host's embedded engine's (internal/ydbflags
// in Ptah), so both name the same flag for the same answer.
func Refused(issues []string) (Gate, bool) {
	gate, _, off := refusal(issues)
	return gate, off
}

// RefusalText is the text of a gate's refusal that issues hold, as YDB wrote
// it, or "" when the refusal is about something else. Each text is YDB's own
// fixed sentence for the flag, measured, and quotes no statement, value or
// path, so a reason may carry it, as the host's embedded engine printed it.
func RefusalText(issues []string) string {
	_, text, _ := refusal(issues)
	return text
}

// refusal is the gate whose refusal issues hold, and that refusal's text.
func refusal(issues []string) (Gate, string, bool) {
	for _, gate := range append(slices.Clone(gates), streaming) {
		for _, refusal := range gate.refusals {
			if slices.ContainsFunc(issues, func(issue string) bool { return strings.Contains(issue, refusal) }) {
				return gate, refusal, true
			}
		}
	}
	return Gate{}, "", false
}

// Clause is what a refusal for want of capability key adds when a feature
// flag decides the key: the flag, and how to have a session report the
// capability, in the words the host's embedded engine writes for a refusal
// the flag causes. A session reads the flags only when the address names the
// monitoring endpoint, and takes its release line's default otherwise, so
// turning the flag on is not enough without it. Clause is empty for a key no
// flag decides.
func Clause(key string) string {
	at := slices.IndexFunc(gates, func(gate Gate) bool { return gate.Key == key })
	if at < 0 {
		return ""
	}
	return fmt.Sprintf("; YDB keeps it behind feature flag %s: turn the flag on, and name the cluster's monitoring endpoint in the URL "+
		"(monitoring=http://host:8765) so Ptah reads the flags before it plans", gates[at].Flag)
}

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
