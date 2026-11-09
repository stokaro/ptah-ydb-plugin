package tablesource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A starting layout by key the parser cannot read, and a column table's
// tiered TTL YDB would not take, are refused where they are written. Ported
// from the host's TestParseSplitPoints_FailurePath and the column storage
// rows of ydbcolumn's TestParse_FailurePath.
func TestTableDirectiveValues_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string]string
		want       string
	}{
		{"an empty layout", map[string]string{"partition_at_keys": ""}, `table "items": invalid partition_at_keys "": name at least one split point, such as 10, 20 or \(10, 'a'\), \(20\)`},
		{"an empty tuple", map[string]string{"partition_at_keys": "10, ()"}, `table "items": invalid partition_at_keys "10, \(\)": expected a value at offset 5`},
		{"an unclosed string", map[string]string{"partition_at_keys": "'abc"}, `table "items": invalid partition_at_keys "'abc": a string value has no closing '`},
		{"two values with no comma", map[string]string{"partition_at_keys": "10 20"}, `table "items": invalid partition_at_keys "10 20": expected a comma after split point 1 at offset 3`},
		{"a trailing comma", map[string]string{"partition_at_keys": "10, 20,"}, `table "items": invalid partition_at_keys "10, 20,": a split point ends without a value`},
		{"a nested tuple", map[string]string{"partition_at_keys": "((10, 20))"}, `table "items": invalid partition_at_keys "\(\(10, 20\)\)": expected a value at offset 1`},
		{"a tier source relative to the database", map[string]string{"store": "COLUMN", "column_ttl": `{"column":"t","tiers":[{"interval":"P1D","external_source":"ext/cold"}]}`},
			`table "items": TTL tier external source must be an absolute database path`},
		{"a deleting tier before a moving one", map[string]string{"store": "COLUMN",
			"column_ttl": `{"column":"t","tiers":[{"interval":"P1D"},{"interval":"P2D","external_source":"/local/ext/cold"}]}`}, `table "items": only the last TTL tier may delete data`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := goRequest(c, tableDeclaration(2, "Item", test.attributes))
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(tables, qt.HasLen, 0)
		})
	}
}

// A global index's partitioning or read replicas YDB would not take is
// refused, the first bad attribute named in the order the directive lists
// them. Ported from the host's ydbindex TestParseDeclaration_FailurePath.
func TestIndexDirectiveValues_FailurePath(t *testing.T) {
	facets := []string{provider.ObservedColumnsFeature, provider.ObservedIndexesFeature, provider.ObservedIndexFacetsFeature}
	for _, test := range []struct {
		name       string
		attributes map[string]string
		want       string
	}{
		{"a partition size of zero", map[string]string{"auto_partitioning_partition_size_mb": "0"}, `index "by_body": invalid auto_partitioning_partition_size_mb "0": write a whole number of at least 1; YDB refuses a count of zero`},
		{"a negative maximum", map[string]string{"auto_partitioning_max_partitions_count": "-1"}, `index "by_body": invalid auto_partitioning_max_partitions_count "-1": write a whole number of at least 1; YDB refuses a count of zero`},
		{"replicas in every zone", map[string]string{"read_replicas_settings": "ALL_AZ:1"}, `index "by_body": invalid read_replicas_settings "ALL_AZ:1": write PER_AZ:<n> for n replicas in every availability zone, or ANY_AZ:<n> for n in all of them together`},
		{"two bad attributes", map[string]string{"auto_partitioning_by_load": "yes", "auto_partitioning_min_partitions_count": "0"}, `index "by_body": invalid auto_partitioning_by_load "yes": write ENABLED or DISABLED`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), facets), goRequest(c, indexDeclaration(2, "Item", test.attributes)))
			c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = `+test.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
}
