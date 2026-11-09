package backend_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// platformContext is a call from a host that reads a table's platform "ydb"
// block.
func platformContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.HCLPlatformFeature})
}

// platformDocument is an HCL document whose platform "ydb" block of
// app/records writes the given overrides, with the models a host that reads
// the block enrolls: those that list HCL and those of the block's claim.
func platformDocument(overrides map[string]string) *protocol.DecodeSourceRequest {
	document := &protocol.SourceDocument{Id: "doc", File: "schema.hcl", Format: protocol.SourceDocument_HCL, ByteLength: 1,
		Bindings: []*protocol.SourceBinding{{Id: "records", Object: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}}}}
	claim := tablesource.HCLPlatformBlock()
	for _, model := range backend.Description().Engines[0].ValueModels {
		if model.Representation == protocol.ValueInput_DESIRED && (slices.Contains(model.SourceFormats, protocol.SourceDocument_HCL) || slices.Contains(claim.Models, model.Kind)) {
			document.Models = append(document.Models, proto.CloneOf(model))
		}
	}
	mapping := &protocol.SourceMapping{}
	for _, attribute := range claim.Attributes {
		if value, written := overrides[attribute.Name]; written {
			mapping.Entries = append(mapping.Entries, &protocol.SourceEntry{Key: attribute.Name, KeySpan: span(), Value: text(value)})
		}
	}
	document.Fragments = []*protocol.SourceFragment{{Id: "platform", Selector: claim.Selector, HclBlock: claim, BindingId: "records", Span: span(),
		Content: &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Span: span(), Value: &protocol.SourceValue_Mapping{Mapping: mapping}}}}}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}}
}

// The engine claims a table's platform "ydb" block in HCL with the row
// deletion overrides, for a host that reads it, and no host that does not
// receives the claim.
func TestDescription_ClaimsTheTablePlatformGroup(t *testing.T) {
	c := testcheck.New(t)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.HCLSourceFeature, provider.HCLPlatformFeature})
	unlisted := provider.NegotiatedDescription(backend.Description(), []string{provider.HCLSourceFeature})

	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	c.Assert(provider.ValidateDescription(unlisted, nil), qt.IsNil)
	c.Assert(slices.ContainsFunc(listed.Engines[0].HclSource.Blocks, func(block *protocol.HCLSourceBlock) bool { return proto.Equal(block, tablesource.HCLPlatformBlock()) }), qt.IsTrue)
	c.Assert(slices.ContainsFunc(unlisted.Engines[0].HclSource.Blocks, func(block *protocol.HCLSourceBlock) bool {
		return block.Site == protocol.HCLSourceBlock_TABLE_PLATFORM
	}), qt.IsFalse)
	c.Assert(listed.Engines[0].Features, qt.Contains, provider.HCLPlatformFeature)
}

// A table's platform "ydb" block declares the TTL the YAML group of the same
// name declares, and the decoding states the table's row-table properties.
func TestHCLPlatformSource_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := platformDocument(map[string]string{"row_deletion_column": "ts", "row_deletion_interval": "P30D"})

	result, err := backend.New().DecodeSource(platformContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, result), qt.IsNil)
	value := result.Documents[0].Fragments[0].Values[0].Value
	c.Assert(value.Value.Kind, qt.Equals, schema.TableProperties)
	c.Assert(string(value.CanonicalProjection), qt.Equals, `{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":2592000}}}`)
	c.Assert(result.Documents[0].Coverage.Subjects[0].Kind, qt.Equals, schema.TableProperties)
}

func TestHCLPlatformSource_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"an interval YDB does not take", map[string]string{"row_deletion_column": "ts", "row_deletion_interval": "30 days"}, `.*row_deletion_interval.*30 days.*`},
		{"a TTL without its interval", map[string]string{"row_deletion_column": "ts"}, `.*a TTL needs the interval after which a row is deleted.*`},
		{"an empty column", map[string]string{"row_deletion_column": " ", "row_deletion_interval": "P1D"}, `.*it is empty; remove it to leave the TTL undeclared.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().DecodeSource(platformContext(c), platformDocument(test.overrides))

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(result, qt.IsNil)
		})
	}
}

// For a host that reads the block, an HCL export writes a table's TTL as the
// block's overrides and reports the settings beside it the block cannot say;
// decoding the block gives the TTL back. A host that does not read it gets the
// facet as a loss.
func TestEncodeSource_WritesATableTTLInHCL(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_HCL, tableFacet(c, `{"keyBloomFilter":"ENABLED","ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`))

	encoded, err := backend.New().EncodeSource(platformContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(platformContext(c))), request, encoded), qt.IsNil)
	parts := provider.EncodedSourceParts(encoded)
	c.Assert(parts, qt.HasLen, 1)
	c.Assert(parts[0].Fragment.GetHclBlock().GetSite(), qt.Equals, protocol.HCLSourceBlock_TABLE_PLATFORM)
	c.Assert(encoded.Losses, qt.HasLen, 1)
	c.Assert(encoded.Losses[0].Message, qt.Equals, "YDB table settings (KEY_BLOOM_FILTER = ENABLED) are not represented in HCL")
	decode := platformDocument(nil)
	decode.Documents[0].Fragments[0].Content = &protocol.SourceFragment_Value{Value: proto.CloneOf(parts[0].Fragment.GetValue())}
	decode.Documents[0].Fragments[0].GetValue().Span = span()
	for _, entry := range decode.Documents[0].Fragments[0].GetValue().GetMapping().GetEntries() {
		entry.KeySpan, entry.Value.Span = span(), span()
	}
	c.Assert(decodedProjections(c, platformContext(c), decode), qt.DeepEquals,
		map[string]string{schema.TableProperties + " app/records": `{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`})
	old, err := backend.New().EncodeSource(encodingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.EncodedSourceParts(old), qt.HasLen, 0)
	c.Assert(old.Losses[0].Message, qt.Equals,
		"the YDB settings of table app/records: HCL has no spelling for a YDB table's partitioning, layout, TTL, column families or column storage")
}

// For a host that reads the block, the empty changefeed and native index sets
// a read reports for a table without either are written by the table's
// platform part, beside its TTL, and no loss names them. A set that holds a
// changefeed is lost by name.
func TestEncodeSource_WritesTheEmptySetsOfATableInHCL(t *testing.T) {
	c := testcheck.New(t)
	row := tableFacet(c, `{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`)
	feeds, indexes := kindFacet(c, schema.ChangefeedProperties, "feeds", `{}`), kindFacet(c, schema.IndexProperties, "indexes", `{}`)
	request := tableRequest(protocol.SourceDocument_HCL, row, feeds, indexes)

	encoded, err := backend.New().EncodeSource(platformContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(platformContext(c))), request, encoded), qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(provider.EncodedSourceParts(encoded), qt.HasLen, 1)
}

// kindFacet is a desired facet of kind with payload on app/records, under id.
func kindFacet(c *qt.C, kind, id, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	model := schema.NativeDesiredModel(kind)
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: table, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: id,
		Value: envelope, NormalizedOwner: proto.CloneOf(table), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}
