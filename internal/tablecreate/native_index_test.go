package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// nativeIndexEnvelope is an addition of a vector or full-text index with the
// payload given, as a host would send it back.
func nativeIndexEnvelope(payload string) *protocol.Envelope {
	model := tablecreate.NativeIndexModel()
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.NativeIndexOperationKind, Owner: owner(), SchemaVersion: 1,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}
}

// The operation carries the index's definition alone: the settings the
// server reports for the tables that implement it are left out, and the
// addition decodes to what was captured.
func TestNativeIndexOperation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	var spec native.IndexIntent
	c.Assert(protojson.Unmarshal([]byte(`{`+docsEmbRead+`}`), &spec), qt.IsNil)
	captured, err := tablecreate.CaptureNativeIndex("add", owner(), &spec)
	c.Assert(err, qt.IsNil)
	c.Assert(captured.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_ADDITIVE)
	c.Assert(captured.OperationSemantics.Reason, qt.Equals, "Adds a vector index; YDB builds it over the rows the table holds")
	decoded, err := tablecreate.DecodeNativeIndex(captured.Value)
	c.Assert(err, qt.IsNil)
	var want native.IndexIntent
	c.Assert(protojson.Unmarshal([]byte(`{`+docsEmb+`}`), &want), qt.IsNil)
	c.Assert(proto.Equal(decoded.Index, &want), qt.IsTrue, qt.Commentf("%v", decoded.Index))
	c.Assert(tablecreate.NativeIndexStatement("app/records", decoded), qt.Equals, addDocsEmbSQL)
}

// An addition a host sends back is validated as the planner validates it:
// it adds one vector or full-text index with its settings and nothing else.
func TestNativeIndexOperation_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, payload, message string
	}{
		{"a global index", `{"index":{"name":"i","indexColumns":["a"],"globalIndex":{}}}`,
			`a YDB index addition adds one vector or full-text index and states no settings of the tables that implement it`},
		{"the settings of an implementation table", `{"index":{` + docsEmbRead + `}}`,
			`a YDB index addition adds one vector or full-text index and states no settings of the tables that implement it`},
		{"a vector index without levels", `{"index":{"name":"v","indexColumns":["emb"],"globalVectorKmeansTreeIndex":{"vectorSettings":` +
			`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"clusters":2}}}}`,
			`invalid YDB index addition: index "v": a vector index's levels are between 1 and 16 .*`},
		{"a full-text index without a tokenizer", `{"index":{` + fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"useFilterLowercase":true}`) + `}}`,
			`invalid YDB index addition: index "ft": a YDB full-text index requires tokenizer`},
		// The model's normalization refuses a repeated column in a desired
		// set; an operation is checked on its own.
		{"a key column twice", `{"index":{` + vectorJSON("v", []string{"gen", "gen", "emb"}, nil, cosine3(2), "") + `}}`,
			`invalid YDB index addition: index "v": it names column "gen" twice`},
		{"a column covered twice", `{"index":{` + vectorJSON("v", []string{"emb"}, []string{"body", "body"}, cosine3(2), "") + `}}`,
			`invalid YDB index addition: index "v": it covers column "body" twice`},
		{"no key column", `{"index":{"name":"v","globalVectorKmeansTreeIndex":{"vectorSettings":` + cosine3(2) + `}}}`,
			`invalid YDB index addition: index "v": the index names no column`},
		{"a field the model does not name", `{"index":{` + docsEmb + `},"held":{}}`, `invalid YDB index addition JSON`},
		{"no index", `{}`, `invalid YDB index addition JSON`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := tablecreate.NormalizeNativeIndex(&protocol.ValueInput{Id: "add", Representation: protocol.ValueInput_OPERATION, Value: nativeIndexEnvelope(test.payload)})
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
