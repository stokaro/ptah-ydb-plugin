package backend_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// differingKeys lists, sorted, the keys whose fact differs between lower and
// upper, a key one of them leaves out included.
func differingKeys(lower, upper map[string]bool) []string {
	var keys []string
	for key, value := range lower {
		if other, stated := upper[key]; !stated || other != value {
			keys = append(keys, key)
		}
	}
	for key := range upper {
		if _, stated := lower[key]; !stated {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// Each release line states the facts of the line above it but for the keys
// its server was measured to answer differently from that line. A key changed
// on one line and forgotten on the next, or a line that drifts by a key
// nobody measured, turns a row red. Ported from the host's
// TestYDBPresets_DifferOnlyWhereTheLinesDid.
func TestReleaseLinesDifferOnlyWhereTheLinesDid_HappyPath(t *testing.T) {
	tests := []struct {
		lower, upper string
		want         []string
	}{
		{lower: "26.1", upper: "26.2", want: []string{"alter_column_default", "full_text_indexes", "local_bloom_indexes", "local_ngram_indexes"}},
		{lower: "25.4", upper: "26.1", want: []string{"add_column_with_default", "changefeed_user_sids", "relative_grant_paths", "vector_bit_type"}},
		{lower: "25.3", upper: "25.4", want: []string{"column_family_cache_mode", "external_data_source_secret_paths", "replication_secret_paths",
			"secrets", "topic_consumer_availability_period"}},
		{lower: "25.2", upper: "25.3", want: []string{"changefeed_schema_changes", "document_type_defaults", "returning_clause", "vector_index_maintained_on_write"}},
		{lower: "25.1", upper: "25.2", want: []string{"changefeed_topic_auto_partitioning", "parameterized_decimal", "small_integer_defaults", "transfers",
			"vector_indexes", "wide_date_time_types"}},
	}
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature, provider.CommonObjectsFeature})
	lines := make(map[string]map[string]bool)
	for _, line := range listed.GetEngines()[0].GetReleaseLines() {
		lines[line.GetLine()] = line.GetCapabilities()
	}
	for _, test := range tests {
		t.Run(test.lower+" below "+test.upper, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(lines[test.lower], qt.Not(qt.HasLen), 0)
			c.Assert(differingKeys(lines[test.lower], lines[test.upper]), qt.DeepEquals, test.want)
		})
	}
}

// The host refuses a foreign key, a generated column, a named NOT NULL, an
// enum, an invisible index, a guarded index drop, a column rename, a generic
// constraint drop, an ALGORITHM clause, a sequence, a function, a trigger, a
// materialized view and a policy by the keys the engine states, which say
// that no line has any of them. Ported from the host's
// TestRender_RefusesByCapability_FailurePath, whose refusals the host now
// makes from these facts.
func TestReleaseLinesStateWhatTheHostRefuses_HappyPath(t *testing.T) {
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature, provider.CommonObjectsFeature})
	for _, line := range listed.GetEngines()[0].GetReleaseLines() {
		t.Run(line.GetLine(), func(t *testing.T) {
			c := testcheck.New(t)
			for _, key := range []string{"foreign_keys", "generated_columns", "named_not_null_constraints", "enum_inline_column", "enum_custom_type",
				"invisible_indexes", "drop_index_if_exists", "rename_column_clause", "drop_constraint_generic", "alter_table_algorithm_lock",
				"sequences", "functions", "triggers", "materialized_views", "row_level_security"} {
				c.Assert(line.GetCapabilities()[key], qt.IsFalse, qt.Commentf("key %s", key))
			}
		})
	}
}
