package embedstore

// White-box testing required: metricSetting reads the metric of a vector
// index a live table service describes, and Store.describe, its only caller,
// opens a table session on a driver before it reads anything, so no offline
// caller reaches it. The live test
// TestEmbeddingGenerationInSourceColumnsSubprocess reads distance=cosine back
// from a server.

import (
	"testing"

	qt "github.com/frankban/quicktest"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Each metric a vector index reports reads as the setting YQL creates it
// with, and a metric the provider does not know as none. Ported from the
// host's TestMetricSetting.
func TestMetricSetting(t *testing.T) {
	for _, test := range []struct {
		metric native.VectorIndexSettings_Metric
		want   string
	}{
		{native.VectorIndexSettings_DISTANCE_MANHATTAN, "distance=manhattan"},
		{native.VectorIndexSettings_SIMILARITY_COSINE, "similarity=cosine"},
		{native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT, "similarity=inner_product"},
		{native.VectorIndexSettings_DISTANCE_COSINE, "distance=cosine"},
		{native.VectorIndexSettings_DISTANCE_EUCLIDEAN, "distance=euclidean"},
		{native.VectorIndexSettings_METRIC_UNSPECIFIED, ""},
	} {
		t.Run(test.metric.String(), func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(metricSetting(test.metric), qt.Equals, test.want)
		})
	}
}
