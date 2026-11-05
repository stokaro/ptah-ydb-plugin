package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// declaredIndexSet normalizes a desired native index set of app/records.
func declaredIndexSet(c *qt.C, payload string) *protocol.Envelope {
	c.Helper()
	model := schema.NativeDesiredModel(schema.IndexProperties)
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexProperties, Owner: tableOwner(),
		SchemaVersion: model.SchemaVersion, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return value
}

// A source declares a vector or full-text index without the tables YDB gives
// it, and the host predicts the observation of the declaration: the index with
// no implementation table, which the comparison and the planner read as the
// tables YDB gives a new index.
func TestDeclaredNativeIndexConvertsToItsObservation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	declared := declaredIndexSet(c, `{"indexes":[{"specification":{"name":"docs_emb","indexColumns":["emb"],"globalVectorKmeansTreeIndex":{`+
		`"vectorSettings":{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":2}}}}]}`)

	observed, err := schema.ConvertNative(declared, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)

	c.Assert(err, qt.IsNil)
	c.Assert(string(observed.Payload), qt.Equals, `{"indexes":[{"description":{"globalVectorKmeansTreeIndex":{"vectorSettings":{"clusters":2,"levels":1,`+
		`"settings":{"metric":"DISTANCE_COSINE","vectorDimension":3,"vectorType":"VECTOR_TYPE_FLOAT"}}},"indexColumns":["emb"],"name":"docs_emb","status":"STATUS_READY"}}]}`)
	normalized, err := schema.Normalize(observed)
	c.Assert(err, qt.IsNil)
	c.Assert(normalized.Payload, qt.DeepEquals, observed.Payload)
}

// observedModelOf is the observed model of kind.
func observedModelOf(kind string) *protocol.ValueModel {
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == kind {
			return candidate
		}
	}
	return nil
}

// A read describes every table that implements an index, so an observed
// index that names some of them is refused, and a global index states its
// one table in either representation.
func TestNativeIndexImplementationTables_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, kind, payload string }{
		{"a vector index with one of its two tables", schema.IndexProperties, `{"indexes":[{"description":{"name":"docs_emb","indexColumns":["emb"],` +
			`"status":"STATUS_READY","globalVectorKmeansTreeIndex":{}},"tables":[{"name":"indexImplLevelTable","description":{"columns":[{"name":"id",` +
			`"type":{"typeId":"UINT64"}}],"primaryKey":["id"]}}]}]}`},
		{"a global index without its table", schema.IndexProperties, `{"indexes":[{"description":{"name":"by_label","indexColumns":["label"],` +
			`"status":"STATUS_READY","globalIndex":{}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			model := observedModelOf(test.kind)
			value, err := schema.Normalize(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: test.kind, Owner: tableOwner(), SchemaVersion: model.SchemaVersion,
				Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(test.payload)})
			c.Assert(err, qt.ErrorMatches, `incomplete YDB index implementation tables`)
			c.Assert(value, qt.IsNil)
		})
	}
}

// A declared local index states no storage, and its predicted observation
// takes the storage YDB places a new index of its kind in: the default
// storage, and the local metadata for a min/max index.
func TestDeclaredLocalIndexesConvertToTheirObservation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	declared := storageEnvelope(c, protocol.ValueInput_DESIRED, `{"hashColumns":["id"],"localIndexesStated":true,"localIndexes":[`+
		`{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.01}},{"name":"mm","columns":["id"],"minMax":{}}]}`)

	observed, err := schema.ConvertNative(declared, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)

	c.Assert(err, qt.IsNil)
	c.Assert(string(observed.Payload), qt.Equals, `{"hashColumns":["id"],"localIndexes":[{"bloom":{"falsePositiveProbability":0.01},"columns":["body"],"name":"bf","storageId":"__DEFAULT"},`+
		`{"columns":["id"],"minMax":{},"name":"mm","storageId":"__LOCAL_METADATA"}]}`)
}
