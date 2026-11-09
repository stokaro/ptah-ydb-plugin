package backend_test

import (
	"bytes"
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexFacet is the desired facet of index name of app/records with payload.
func indexFacet(c *qt.C, name, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	index := &protocol.Reference{Engine: "ydb", Kind: "index", Schema: "app", Path: []string{"records"}, Name: name}
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: index, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: "index." + name,
		Value: envelope, NormalizedOwner: proto.CloneOf(index), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

// groupedEncodingContext is the context of a call from a host that lists every
// source feature, writes encoding groups and reads index facets.
func groupedEncodingContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.EncodeGroupsFeature,
		provider.ObservedColumnsFeature, provider.ObservedIndexesFeature, provider.ObservedIndexFacetsFeature})
}

// YAML, Go and HCL write a global index's settings as its index directive's
// attributes, its index entry's keys or its index block's attributes, and
// decoding them gives each facet back. An index that holds the settings YDB gives a new one is written as
// nothing.
func TestEncodeSource_WritesIndexSettings_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			load := `{"partitioningSettings":{"minPartitionsCount":"2","partitioningByLoad":"ENABLED"}}`
			replicas := `{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`
			request := tableRequest(format, indexFacet(c, "by_body", load), indexFacet(c, "by_n", replicas), indexFacet(c, "by_id", `{}`))
			encoded, err := backend.New().EncodeSource(groupedEncodingContext(c), request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(groupedEncodingContext(c))), request, encoded), qt.IsNil)
			c.Assert(decodedProjections(c, groupedEncodingContext(c), sourceDocument(format, request, encoded)), qt.DeepEquals, map[string]string{
				schema.IndexSettingsProperties + " app/by_body": load,
				schema.IndexSettingsProperties + " app/by_n":    replicas,
			})
		})
	}
}
