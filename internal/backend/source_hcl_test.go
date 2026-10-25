package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// span is a position in a one-byte document; every written part of a
// fragment carries one.
func span() *protocol.SourceSpan {
	return &protocol.SourceSpan{Start: &protocol.SourcePosition{Line: 1, Column: 1}, End: &protocol.SourcePosition{ByteOffset: 1, Line: 1, Column: 2}}
}

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Span: span(), Value: &protocol.SourceValue_Text{Text: value}}
}

// hclNodes is an HCL document of coordination_node blocks, each labeled by
// labels and holding settings, with the document's header limits.
func hclNodes(limits []*protocol.SchemaCoverageLimit, blocks ...*protocol.SourceBlock) *protocol.DecodeSourceRequest {
	document := &protocol.SourceDocument{Id: "doc", File: "schema.hcl", Format: protocol.SourceDocument_HCL, ByteLength: 1,
		Models: []*protocol.ValueModel{schema.CoordinationDesiredModel()}, Limits: limits}
	for i, block := range blocks {
		block.Type = "coordination_node"
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + i)), Selector: "coordination_node",
			HclBlock: schema.CoordinationHCLBlock(), Span: span(), Content: &protocol.SourceFragment_Block{Block: block}})
	}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}}
}

func labeled(settings map[string]string, labels ...string) *protocol.SourceBlock {
	block := &protocol.SourceBlock{Span: span(), Attributes: &protocol.SourceMapping{}}
	for _, label := range labels {
		block.Labels = append(block.Labels, &protocol.SourceLabel{Value: label, Span: span()})
	}
	for _, name := range schema.CoordinationSettingNames() {
		if value, set := settings[name]; set {
			block.Attributes.Entries = append(block.Attributes.Entries, &protocol.SourceEntry{Key: name, KeySpan: span(), Value: text(value)})
		}
	}
	return block
}

// A coordination_node block declares the node the Go directive and the YAML
// entry declare, named by its labels. A ptah:not-described header names the
// nodes the host leaves outside the document's claim.
func TestCoordinationHCLSource_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := hclNodes([]*protocol.SchemaCoverageLimit{{Kind: "coordination_node", Name: "app/legacy"}},
		labeled(map[string]string{"self_check_period": "PT0.5S", "read_consistency_mode": "STRICT"}, "app/workers", "queue"),
		labeled(nil, "plain"))
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, result), qt.IsNil)
	queue := result.Documents[0].Fragments[0].Values[0].Value.Value
	c.Assert(queue.Owner.Schema+"/"+queue.Owner.Name, qt.Equals, "app/workers/queue")
	c.Assert(string(queue.Payload), qt.Equals, `{"read_consistency_mode":"strict","self_check_period_millis":500}`)
	plain := result.Documents[0].Fragments[1].Values[0].Value.Value
	c.Assert(plain.Owner.Schema+"|"+plain.Owner.Name+"|"+string(plain.Payload), qt.Equals, "|plain|{}")
	// The host applies the header's limits to the document's claim.
	c.Assert(result.Documents[0].Coverage.Kinds[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
}

func TestCoordinationHCLSource_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request *protocol.DecodeSourceRequest
		want    string
	}{
		{"no label", hclNodes(nil, labeled(nil)), `.*a coordination_node block is labeled with its name, or its directory and its name.*`},
		{"three labels", hclNodes(nil, labeled(nil, "a", "b", "c")), `.*a coordination_node block is labeled with its name, or its directory and its name.*`},
		{"a setting YDB refuses", hclNodes(nil, labeled(map[string]string{"session_grace_period": "PT31S"}, "queue")), `.*coordination node "queue": .*`},
		{"a name YDB refuses", hclNodes(nil, labeled(nil, "ptah_locks")), `.*coordination node "ptah_locks": .*`},
		{"a limit no node can take", hclNodes([]*protocol.SchemaCoverageLimit{{Kind: "coordination_node", Name: "/absolute/node"}}), `.*coverage of github.com/stokaro/ptah-ydb-plugin: .*`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := backend.New().DecodeSource(c.Context(), tc.request)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(result, qt.IsNil)
		})
	}
	t.Run("a block of another claim", func(t *testing.T) {
		c := qt.New(t)
		request := hclNodes(nil, labeled(nil, "queue"))
		claim := proto.CloneOf(schema.CoordinationHCLBlock())
		claim.Naming = protocol.HCLSourceBlock_JOINED_NAME
		request.Documents[0].Fragments[0].HclBlock = claim
		result, err := backend.New().DecodeSource(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*HCL fragment changed its negotiated block claim.*`)
		c.Assert(result, qt.IsNil)
	})
}

// An index entry's YAML keys are claimed so that each is refused by name, as
// the index directive's attributes are.
func TestIndexYAMLKeysAreRefusedByName(t *testing.T) {
	c := qt.New(t)
	claim := tablesource.IndexYAMLKeys()[0]
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Name: "records"}
	document := &protocol.SourceDocument{Id: "doc", File: "schema.yaml", Format: protocol.SourceDocument_YAML, ByteLength: 1,
		Models:   []*protocol.ValueModel{schema.TableDesiredModel()},
		Bindings: []*protocol.SourceBinding{{Id: "records", EntryKey: "records", Object: table}},
		Fragments: []*protocol.SourceFragment{{Id: "a", Selector: claim.Selector, YamlKey: claim, BindingId: "records", Span: span(),
			Subject: &protocol.Reference{Engine: "ydb", Kind: "index", Name: "by_ts", Path: []string{"records"}},
			Content: &protocol.SourceFragment_Value{Value: text("ENABLED")}}}}
	result, err := backend.New().DecodeSource(c.Context(), &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}})
	c.Assert(err, qt.ErrorMatches, `.*`+claim.Key+` is not supported by the YDB provider yet: an index states no settings of its own in the row-table model.*`)
	c.Assert(result, qt.IsNil)
}

// Both claims are two-way: a host that does not list a feature gets none of
// its claims.
func TestSourceClaimsAreNegotiated(t *testing.T) {
	c := qt.New(t)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature}).Engines[0]
	c.Assert(proto.Equal(listed.HclSource.Blocks[0], schema.CoordinationHCLBlock()), qt.IsTrue)
	c.Assert(listed.HclSource.HeaderKinds, qt.DeepEquals, []string{"coordination_node"})
	index := 0
	for _, claim := range listed.YamlSourceKeys {
		if claim.Scope == protocol.YAMLSourceKey_INDEX {
			index++
		}
	}
	c.Assert(index, qt.Equals, len(tablesource.IndexYAMLKeys()))
	unlisted := provider.NegotiatedDescription(backend.Description(), nil).Engines[0]
	c.Assert(unlisted.HclSource, qt.IsNil)
	for _, claim := range unlisted.YamlSourceKeys {
		c.Assert(claim.Scope, qt.Not(qt.Equals), protocol.YAMLSourceKey_INDEX)
	}
	c.Assert(unlisted.Features, qt.Not(qt.Contains), provider.HCLSourceFeature)
}
