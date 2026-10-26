//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// offlineSpan is a position in a one-byte document.
func offlineSpan() *protocol.SourceSpan {
	return &protocol.SourceSpan{Start: &protocol.SourcePosition{Line: 1, Column: 1}, End: &protocol.SourcePosition{ByteOffset: 1, Line: 1, Column: 2}}
}

// hclNode is an HCL document holding the coordination_node block app/queue.
func hclNode(claim *protocol.HCLSourceBlock) *protocol.DecodeSourceRequest {
	block := &protocol.SourceBlock{Type: "coordination_node", Span: offlineSpan(), Labels: []*protocol.SourceLabel{{Value: "app", Span: offlineSpan()}, {Value: "queue", Span: offlineSpan()}},
		Attributes: &protocol.SourceMapping{Entries: []*protocol.SourceEntry{{Key: "session_grace_period", KeySpan: offlineSpan(),
			Value: &protocol.SourceValue{Span: offlineSpan(), Value: &protocol.SourceValue_Text{Text: "PT12.5S"}}}}}}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{{Id: "doc", File: "schema.hcl", Format: protocol.SourceDocument_HCL, ByteLength: 1,
		Models: []*protocol.ValueModel{schema.CoordinationDesiredModel()}, Fragments: []*protocol.SourceFragment{{Id: "node", Selector: "coordination_node",
			HclBlock: claim, Span: offlineSpan(), Content: &protocol.SourceFragment_Block{Block: block}}}}}}
}

// The HCL claim and the index entry keys are two-way additions of contract
// 25.3, and source.encode is a service a host of 25.3 calls: a host that lists
// source.hcl and source.entry-keys decodes a coordination_node block and gets
// the index keys, the encoding writes the node back as the same block, and a
// host that lists neither gets no HCL claim and no index key. No database
// session is opened.
func TestSourceClaimsOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	listing := startProviderFor(c, provider.HCLSourceFeature, provider.EntryKeysFeature)
	engine := listing.Description().Engines[0]
	c.Assert(engine.Features, qt.Contains, provider.EncodeSourceFeature)
	c.Assert(proto.Equal(engine.HclSource.Blocks[0], schema.CoordinationHCLBlock()), qt.IsTrue)
	index := 0
	for _, claim := range engine.YamlSourceKeys {
		if claim.Scope == protocol.YAMLSourceKey_INDEX {
			index++
		}
	}
	c.Assert(index, qt.Equals, len(tablesource.IndexYAMLKeys()))

	decoded, err := listing.DecodeSource(c.Context(), hclNode(schema.CoordinationHCLBlock()))
	c.Assert(err, qt.IsNil)
	value := decoded.Documents[0].Fragments[0].Values[0]
	c.Assert(value.Value.Value.Owner.Schema+"/"+value.Value.Value.Owner.Name, qt.Equals, "app/queue")
	c.Assert(string(value.Value.CanonicalProjection), qt.Equals, `{"session_grace_period_millis":12500}`)

	encoded, err := listing.EncodeSource(c.Context(), &protocol.EncodeSourceRequest{Engine: "ydb", Format: protocol.SourceDocument_HCL, TargetFeatures: engine.Features,
		Values: []*protocol.EncodeSourceValue{{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	block := encoded.Fragments[0].Fragment.GetBlock()
	c.Assert([]string{block.Labels[0].Value, block.Labels[1].Value, block.Attributes.Entries[0].Key, block.Attributes.Entries[0].Value.GetText()}, qt.DeepEquals,
		[]string{"app", "queue", "session_grace_period", "PT12.5S"})

	unlisted := startProviderFor(c)
	c.Assert(unlisted.Description().Engines[0].HclSource, qt.IsNil)
	for _, claim := range unlisted.Description().Engines[0].YamlSourceKeys {
		c.Assert(claim.Scope, qt.Not(qt.Equals), protocol.YAMLSourceKey_INDEX)
	}
	refused, err := unlisted.DecodeSource(c.Context(), hclNode(schema.CoordinationHCLBlock()))
	c.Assert(err, qt.ErrorMatches, `.*HCL fragment claims a block on an engine that declares none.*`)
	c.Assert(refused, qt.IsNil)
}
