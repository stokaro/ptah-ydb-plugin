package backend_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// node is the desired value of a coordination node.
func node(c *qt.C, id, directory, name string, intent schema.CoordinationIntent) *protocol.EncodeSourceValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: directory, Name: name}
	value, err := schema.CaptureCoordinationIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: id, Value: value,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.CoordinationDesiredModel().CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// encodingContext is the context of a call from a host that lists every
// source feature.
func encodingContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature})
}

func encodeRequest(format protocol.SourceDocument_Format, values ...*protocol.EncodeSourceValue) *protocol.EncodeSourceRequest {
	return &protocol.EncodeSourceRequest{Engine: "ydb", Format: format, Values: values}
}

// sourceDocument is the document a host would decode the parts of an
// encoding in, every part of it given a position, with the request's
// bindings and the engine's desired models of the format.
func sourceDocument(format protocol.SourceDocument_Format, request *protocol.EncodeSourceRequest, encoded *protocol.EncodedSource) *protocol.DecodeSourceRequest {
	document := &protocol.SourceDocument{Id: "doc", File: "exported", Format: format, ByteLength: 1, Bindings: request.GetBindings()}
	for _, model := range backend.Description().Engines[0].ValueModels {
		if model.Representation == protocol.ValueInput_DESIRED && slices.Contains(model.SourceFormats, format) {
			document.Models = append(document.Models, proto.CloneOf(model))
		}
	}
	var place func(*protocol.SourceValue)
	place = func(value *protocol.SourceValue) {
		value.Span = span()
		for _, entry := range value.GetMapping().GetEntries() {
			entry.KeySpan = span()
			place(entry.Value)
		}
		for _, item := range value.GetSequence().GetValues() {
			place(item)
		}
	}
	for _, part := range provider.EncodedSourceParts(encoded) {
		positioned := proto.CloneOf(part.Fragment)
		positioned.Span = span()
		if value := positioned.GetValue(); value != nil {
			place(value)
		}
		if block := positioned.GetBlock(); block != nil {
			block.Span = span()
			for _, label := range block.Labels {
				label.Span = span()
			}
			for _, entry := range block.GetAttributes().GetEntries() {
				entry.KeySpan = span()
				place(entry.Value)
			}
		}
		document.Fragments = append(document.Fragments, positioned)
	}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}}
}

// decodedProjections are the canonical projections of what decoding the
// parts gives, by the owner's directory and name.
func decodedProjections(c *qt.C, ctx context.Context, request *protocol.DecodeSourceRequest) map[string]string {
	c.Helper()
	decoded, err := backend.New().DecodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	projections := map[string]string{}
	for _, fragment := range decoded.Documents[0].Fragments {
		for _, value := range fragment.Values {
			owner := value.Value.Value.Owner
			projections[value.Value.Value.Kind+" "+owner.Schema+"/"+owner.Name] = string(value.Value.CanonicalProjection)
		}
	}
	return projections
}

// Each format writes the nodes as the fragments its claim reads, and decoding
// them gives every node back with its canonical projection.
func TestEncodeSource_CoordinationNodes(t *testing.T) {
	grace, check := uint32(12_500), uint32(1_000)
	strict, detailed := "strict", "detailed"
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			values := []*protocol.EncodeSourceValue{
				node(c, "queue", "app/workers", "queue", schema.CoordinationIntent{SessionGracePeriodMillis: &grace, SelfCheckPeriodMillis: &check,
					ReadConsistencyMode: &strict, RateLimiterCountersMode: &detailed}),
				node(c, "plain", "", "plain", schema.CoordinationIntent{}),
			}
			request := encodeRequest(format, values...)
			encoded, err := backend.New().EncodeSource(encodingContext(c), request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(encodingContext(c))), request, encoded), qt.IsNil)

			c.Assert(decodedProjections(c, encodingContext(c), sourceDocument(format, request, encoded)), qt.DeepEquals, map[string]string{
				schema.CoordinationProperties + " app/workers/queue": string(values[0].Value.CanonicalProjection),
				schema.CoordinationProperties + " /plain":            string(values[1].Value.CanonicalProjection),
			})
		})
	}
}

// A YAML source records the struct a node was declared on; the other formats
// leave that source provenance out.
func TestEncodeSource_WritesWhatTheFormatHolds(t *testing.T) {
	c := testcheck.New(t)
	value := node(c, "queue", "", "queue", schema.CoordinationIntent{StructName: "Worker"})
	yaml, err := backend.New().EncodeSource(encodingContext(c), encodeRequest(protocol.SourceDocument_YAML, value))
	c.Assert(err, qt.IsNil)
	entries := yaml.Fragments[0].Fragment.GetValue().GetMapping().GetEntries()[0].GetValue().GetMapping().GetEntries()
	c.Assert(entries[len(entries)-1].Key+"="+entries[len(entries)-1].GetValue().GetText(), qt.Equals, "struct_name=Worker")
	hcl, err := backend.New().EncodeSource(encodingContext(c), encodeRequest(protocol.SourceDocument_HCL, value))
	c.Assert(err, qt.IsNil)
	c.Assert(hcl.Fragments[0].Fragment.GetBlock().GetAttributes().GetEntries(), qt.HasLen, 0)
	c.Assert(hcl.Fragments[0].Placement, qt.Equals, protocol.EncodedFragment_AFTER_TABLES)
}

// tableFacet is the desired row-table facet of app/records with payload.
func tableFacet(c *qt.C, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	model := schema.TableDesiredModel()
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: table, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: "records",
		Value: envelope, NormalizedOwner: proto.CloneOf(table), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

func tableRequest(format protocol.SourceDocument_Format, values ...*protocol.EncodeSourceValue) *protocol.EncodeSourceRequest {
	request := encodeRequest(format, values...)
	request.Bindings = []*protocol.SourceBinding{{Id: "records", EntryKey: "records", StructName: "Record",
		Object: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}}}
	return request
}

// HCL has no spelling for a table's YDB settings, as the host's embedded
// engine reports, so a table's facet is lost there with a line that names the
// table.
func TestEncodeSource_ReportsTableSettingsAsHCLLosses(t *testing.T) {
	c := testcheck.New(t)
	encoded, err := backend.New().EncodeSource(encodingContext(c), tableRequest(protocol.SourceDocument_HCL, tableFacet(c, `{"keyBloomFilter":"ENABLED"}`)))
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Fragments, qt.HasLen, 0)
	c.Assert(encoded.Losses, qt.HasLen, 1)
	c.Assert(encoded.Losses[0].Message, qt.Equals,
		"the YDB settings of table app/records: HCL has no spelling for a YDB table's partitioning, layout, TTL, column families or column storage")
}

// YAML and Go write a table's settings, and decoding them gives the facet
// back; one setting is one part, which a host without encoding groups takes
// as a flat fragment.
func TestEncodeSource_WritesTableSettings_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			request := tableRequest(format, tableFacet(c, `{"keyBloomFilter":"ENABLED"}`))
			encoded, err := backend.New().EncodeSource(encodingContext(c), request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(encoded.Fragments, qt.HasLen, 1)
			c.Assert(decodedProjections(c, encodingContext(c), sourceDocument(format, request, encoded)), qt.DeepEquals,
				map[string]string{schema.TableProperties + " app/records": `{"keyBloomFilter":"ENABLED"}`})
		})
	}
}

// A request the SDK refuses, such as a format the host does not write, is
// refused before anything is encoded.
func TestEncodeSource_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	encoded, err := backend.New().EncodeSource(encodingContext(c), encodeRequest(protocol.SourceDocument_SQL))
	c.Assert(err, qt.ErrorMatches, `.*a format the host writes is YAML, HCL or Go annotations.*`)
	c.Assert(encoded, qt.IsNil)
}

func TestEncodeSource_ReportsAccessConstraintsAsLosses(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			value := node(c, "queue", "app", "queue", schema.CoordinationIntent{Access: &native.AccessIntent{Owner: new("alice")}})
			encoded, err := backend.New().EncodeSource(encodingContext(c), encodeRequest(format, value))
			c.Assert(err, qt.IsNil)
			c.Assert(len(encoded.Fragments), qt.Equals, 0)
			c.Assert(encoded.Losses, qt.HasLen, 1)
			c.Assert(encoded.Losses[0].ValueId, qt.Equals, "queue")
			c.Assert(encoded.Losses[0].Message, qt.Equals, "the access requirement of coordination node app/queue has no source spelling")
		})
	}
}

func TestEncodeSource_RefusesIncompleteCoverage(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			for _, knowledge := range []protocol.ModelKnowledge_State{protocol.ModelKnowledge_UNINSPECTED, protocol.ModelKnowledge_UNREPRESENTABLE} {
				t.Run(knowledge.String(), func(t *testing.T) {
					c := testcheck.New(t)
					request := encodeRequest(format)
					request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED,
						Kinds: []*protocol.ModelKindCoverage{{Model: schema.CoordinationDesiredModel(), Knowledge: &protocol.ModelKnowledge{State: knowledge, Reason: "the source leaves nodes unmanaged"}}}}
					encoded, err := backend.New().EncodeSource(encodingContext(c), request)
					c.Assert(err, qt.ErrorMatches, `.*YDB source encoding cannot preserve incomplete coverage of .*coordination-node.*`)
					c.Assert(encoded, qt.IsNil)
				})
			}
		})
	}
}

func TestEncodeSource_RefusesIncompleteSubjectCoverage(t *testing.T) {
	c := testcheck.New(t)
	value := node(c, "queue", "app", "queue", schema.CoordinationIntent{})
	request := encodeRequest(protocol.SourceDocument_HCL, value)
	request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED,
		Kinds: []*protocol.ModelKindCoverage{{Model: schema.CoordinationDesiredModel(), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}},
		Subjects: []*protocol.ModelSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(value.Value.Value.Owner),
			NormalizedSubject: proto.CloneOf(value.Value.Value.Owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "node not read"}}}}
	encoded, err := backend.New().EncodeSource(encodingContext(c), request)
	c.Assert(err, qt.ErrorMatches, `.*YDB source encoding cannot preserve incomplete coverage of .* app/queue.*`)
	c.Assert(encoded, qt.IsNil)
	request.Coverage.Subjects[0].Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
	encoded, err = backend.New().EncodeSource(encodingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Fragments, qt.HasLen, 1)
}

func TestEncodeSource_Canceled(t *testing.T) {
	c := testcheck.New(t)
	ctx, cancel := context.WithCancel(encodingContext(c))
	cancel()
	encoded, err := backend.New().EncodeSource(ctx, encodeRequest(protocol.SourceDocument_HCL))
	c.Assert(err, qt.ErrorMatches, `.*context canceled.*`)
	c.Assert(encoded, qt.IsNil)
}
