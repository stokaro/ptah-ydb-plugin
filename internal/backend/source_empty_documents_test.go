package backend_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// emptyDocumentClaims decodes a document of format that declares nothing, as
// a host sends an empty Go file or directory, an empty YAML document or an
// empty HCL document: no fragment, and the engine's desired models of the
// format. It returns which native namespaces the document claims complete.
func emptyDocumentClaims(c *qt.C, format protocol.SourceDocument_Format) map[string]bool {
	c.Helper()
	document := &protocol.SourceDocument{Id: "empty", File: "empty", Format: format}
	for _, model := range backend.Description().Engines[0].ValueModels {
		if model.Representation == protocol.ValueInput_DESIRED && slices.Contains(model.SourceFormats, format) {
			document.Models = append(document.Models, proto.CloneOf(model))
		}
	}
	decoded, err := backend.New().DecodeSource(c.Context(), &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}})
	c.Assert(err, qt.IsNil)
	c.Assert(decoded.Documents[0].Fragments, qt.HasLen, 0)
	c.Assert(decoded.Documents[0].Coverage.Representation, qt.Equals, protocol.ValueInput_DESIRED)
	claims := make(map[string]bool)
	for _, kind := range []string{schema.CoordinationProperties, schema.ChangefeedProperties, schema.StreamingQueryProperties,
		schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties} {
		claims[kind] = slices.ContainsFunc(decoded.Documents[0].Coverage.Kinds, func(claimed *protocol.ModelKindCoverage) bool {
			return claimed.GetModel().GetKind() == kind && claimed.GetKnowledge().GetState() == protocol.ModelKnowledge_COMPLETE
		})
	}
	return claims
}

// A Go or YAML document that declares nothing claims every native namespace
// it can declare complete: the coordination nodes, the changefeeds, the
// streaming queries, the resource pools and the classifiers, so the schema it
// describes has none. An HCL document cannot declare changefeeds, streaming
// queries, pools or classifiers and leaves them uninspected; it declares
// coordination nodes through the provider's block, so it claims them. Ported
// from the host's TestEmptySourcesRecordSupportedFeatureNamespaces.
func TestEmptySourceDocumentsClaimTheirNamespaces_HappyPath(t *testing.T) {
	every := map[string]bool{schema.CoordinationProperties: true, schema.ChangefeedProperties: true, schema.StreamingQueryProperties: true,
		schema.ResourcePoolProperties: true, schema.ResourcePoolClassifierProperties: true}
	for _, test := range []struct {
		format protocol.SourceDocument_Format
		want   map[string]bool
	}{
		{format: protocol.SourceDocument_GO_ANNOTATIONS, want: every},
		{format: protocol.SourceDocument_YAML, want: every},
		{format: protocol.SourceDocument_HCL, want: map[string]bool{schema.CoordinationProperties: true, schema.ChangefeedProperties: false,
			schema.StreamingQueryProperties: false, schema.ResourcePoolProperties: false, schema.ResourcePoolClassifierProperties: false}},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			c := testcheck.New(t)

			c.Assert(emptyDocumentClaims(c, test.format), qt.DeepEquals, test.want)
		})
	}
}
