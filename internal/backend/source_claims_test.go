package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's TestYAMLDocument_DescribesSecrets and
// TestRoundTrip_ExternalObjectsMakeNoHCLClaim: the YAML surface has keys for
// secrets and for external objects, so a YAML document that holds none of
// them still claims each kind it enrolls, completely, and a secret or an
// external object it leaves out is one it asks to go.
func TestDecodeSourceClaimsTheKindsOfAYAMLDocumentWithoutTheirKeys(t *testing.T) {
	c := testcheck.New(t)
	kinds := []string{schema.SecretProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties}
	document := &protocol.SourceDocument{Id: "doc", File: "schema.yaml", ByteLength: 7, Format: protocol.SourceDocument_YAML}
	for _, kind := range kinds {
		document.Models = append(document.Models, schema.NativeDesiredModel(kind))
	}

	result, err := backend.New().DecodeSource(c.Context(), &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}})

	c.Assert(err, qt.IsNil)
	claims := make(map[string]protocol.ModelKnowledge_State)
	for _, claim := range result.Documents[0].Coverage.Kinds {
		claims[claim.Model.Kind] = claim.Knowledge.State
	}
	c.Assert(claims, qt.DeepEquals, map[string]protocol.ModelKnowledge_State{schema.SecretProperties: protocol.ModelKnowledge_COMPLETE,
		schema.ExternalSourceProperties: protocol.ModelKnowledge_COMPLETE, schema.ExternalTableProperties: protocol.ModelKnowledge_COMPLETE})
	c.Assert(result.Documents[0].Coverage.Subjects, qt.HasLen, 0)
}
