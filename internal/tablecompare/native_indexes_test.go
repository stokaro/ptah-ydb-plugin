package tablecompare_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// heldVector is a table's native index set holding one vector index, as a
// read reports it with the two tables that implement it.
const heldVector = `{"indexes":[{"description":{"name":"by_emb","indexColumns":["emb"],"status":"STATUS_READY","globalVectorKmeansTreeIndex":{` +
	`"vectorSettings":{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"clusters":2,"levels":1}}},"tables":[` +
	`{"name":"indexImplLevelTable","description":{"columns":[{"name":"__ydb_id","type":{"typeId":"UINT64"}}],"primaryKey":["__ydb_id"]}},` +
	`{"name":"indexImplPostingTable","description":{"columns":[{"name":"__ydb_parent","type":{"typeId":"UINT64"}},{"name":"id","type":{"typeId":"UINT64"}}],` +
	`"primaryKey":["__ydb_parent","id"]}}]}]}`

// A source of a host that negotiates source.index-kinds declares a table's
// vector and full-text indexes, so one that states the native index sets of
// every table it declares and writes none for this one declares it to have
// none: the vector index the table holds goes, as on the host's embedded
// engine.
func TestCompare_ReportsAnUnstatedNativeIndexSet_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	compared := request(c, side{}, side{indexes: heldVector})
	compared.TargetFeatures = []string{provider.IndexKindsFeature}

	result := compare(c, compared)

	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Changes[0].Kind, qt.Equals, schema.IndexProperties)
	c.Assert(result.Changes[0].Subject.Kind, qt.Equals, "table")
	c.Assert(string(result.Changes[0].Value.Value.Payload), qt.Contains, `"facet":"`+schema.IndexProperties+`"`)
}

// Without the feature a source cannot declare a vector or full-text index, and
// a source that could not describe the native index sets states nothing: the
// table keeps the index it holds.
func TestCompare_KeepsAnUnstatedNativeIndexSet(t *testing.T) {
	for _, test := range []struct {
		name     string
		desired  side
		features []string
	}{
		{"a host without index kinds", side{}, nil},
		{"a source that could not describe the native index sets", side{uninspected: true}, []string{provider.IndexKindsFeature}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			compared := request(c, test.desired, side{indexes: heldVector})
			compared.TargetFeatures = test.features

			result := compare(c, compared)

			c.Assert(result.Changes, qt.HasLen, 0)
		})
	}
}
