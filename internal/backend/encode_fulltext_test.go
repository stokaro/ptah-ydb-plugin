package backend_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// unnormalizedFacet is a desired index set of app/records carrying payload
// as written, which no normalization has checked.
func unnormalizedFacet(payload string) *protocol.EncodeSourceValue {
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	model := schema.NativeDesiredModel(schema.IndexProperties)
	envelope := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: table, Payload: []byte(payload)}
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: "facet",
		Value: envelope, NormalizedOwner: proto.CloneOf(table), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

// A full-text index option the provider does not know is refused, never left
// out of the export: the index's settings are a typed model, and a value
// naming another option is not one. Ported from the host's
// TestRender_IndexOptions_RefusesAFullTextOptionItCannotWrite.
func TestEncodeSource_RefusesAFullTextOptionItDoesNotKnow_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, unnormalizedFacet(
		`{"indexes":[{"specification":{"name":"ft","indexColumns":["body"],"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[{"column":"body",`+
			`"analyzers":{"tokenizer":"STANDARD","flavor":"sweet"}}]}}}}]}`))

	encoded, err := backend.New().EncodeSource(indexKindsContext(c), request)

	c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = invalid desired native YDB JSON`)
	c.Assert(encoded, qt.IsNil)
}

// Full-text settings a directive cannot write so that it reads them back, two
// columns analyzed in two ways, are reported as a loss of the table rather
// than written without one of them.
func TestEncodeSource_LosesFullTextSettingsItCannotWrite(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, tableModelFacet(c, "facet", schema.IndexProperties,
		`{"indexes":[{"specification":{"name":"ft","indexColumns":["body","title"],"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[`+
			`{"column":"body","analyzers":{"tokenizer":"STANDARD"}},{"column":"title","analyzers":{"tokenizer":"WHITESPACE"}}]}}}}]}`))

	encoded, err := backend.New().EncodeSource(indexKindsContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 1)
	c.Assert(encoded.Losses[0].Message, qt.Equals, "table app/records: its YDB settings cannot be written so that a source reads them back unchanged")
}
