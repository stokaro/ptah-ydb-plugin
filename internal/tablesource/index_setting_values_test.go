package tablesource_test

import (
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// vectorIndex is a vector index directive with every setting, each of
// changes replacing the setting it names.
func vectorIndex(changes map[string]string) map[string]string {
	attributes := map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "distance": "cosine", "vector_type": "float",
		"vector_dimension": "3", "levels": "2", "clusters": "8"}
	maps.Copy(attributes, changes)
	return attributes
}

// A vector or full-text index setting YDB does not take is refused where it
// is written, naming the setting. Ported from the host's
// TestParseSource_VectorIndex_FailurePath, TestParse_YDBVectorIndex_FailurePath,
// TestParseVectorDeclaration_FailurePath and
// TestFullTextOptionsRefuseUnknownOrMalformedValues.
func TestIndexKindSettingValues_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string]string
		want       string
	}{
		{"inner product as a distance", vectorIndex(map[string]string{"distance": "inner_product"}), `table "items": index "v": invalid distance "inner_product": write one of cosine, euclidean, manhattan`},
		{"euclidean as a similarity", map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "similarity": "euclidean", "vector_type": "float",
			"vector_dimension": "3", "levels": "2", "clusters": "8"}, `table "items": index "v": invalid similarity "euclidean": write one of inner_product, cosine`},
		{"an element type YDB does not have", vectorIndex(map[string]string{"vector_type": "half"}), `table "items": index "v": invalid vector_type "half": write one of float, uint8, int8, bit`},
		{"an empty element type", vectorIndex(map[string]string{"vector_type": ""}), `table "items": index "v": invalid vector_type "": write one of float, uint8, int8, bit`},
		{"a dimension of zero", vectorIndex(map[string]string{"vector_dimension": "0"}), `table "items": index "v": invalid vector_dimension "0": write a whole number of at least 1; YDB refuses a count of zero`},
		{"a dimension that is no number", vectorIndex(map[string]string{"vector_dimension": "three"}), `table "items": index "v": invalid vector_dimension "three": write a whole number of at least 1; YDB refuses a count of zero`},
		{"levels that are no number", vectorIndex(map[string]string{"levels": "deep"}), `table "items": index "v": invalid levels "deep": write a whole number of at least 1; YDB refuses a count of zero`},
		{"clusters of zero", vectorIndex(map[string]string{"clusters": "0"}), `table "items": index "v": invalid clusters "0": write a whole number of at least 1; YDB refuses a count of zero`},
		{"an unknown tokenizer", map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain", "tokenizer": "icu"}, `table "items": index "ft": YDB full-text index option tokenizer: unknown tokenizer "icu"`},
		{"a flag that is no boolean", map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain", "tokenizer": "standard", "use_filter_lowercase": "yes"},
			`table "items": index "ft": YDB full-text index option use_filter_lowercase: "yes" is not true or false`},
		{"SQL inside a number", map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain", "tokenizer": "standard", "use_filter_length": "true",
			"filter_length_min": "1); DROP TABLE t; --"}, `table "items": index "ft": YDB full-text index option filter_length_min: "1\); DROP TABLE t; --" is not a nonnegative 32-bit integer`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := goRequest(c, indexDirective(2, test.attributes))
			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.IndexKindsFeature}), request)
			c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = `+test.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
}
