package backend_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A full-text index's n-gram filter and its two lengths are written and read
// back. Ported from the host's TestRender_FullTextIndexFromYAML.
func TestEncodeSource_WritesAFullTextIndexsNgramFilter(t *testing.T) {
	c := testcheck.New(t)
	ctx := indexKindsContext(c)
	ngram := `{"indexes":[{"specification":{"name":"ft","indexColumns":["body"],"globalFulltextRelevanceIndex":{"fulltextSettings":{"columns":[{"column":"body",` +
		`"analyzers":{"tokenizer":"STANDARD","useFilterLowercase":true,"useFilterNgram":true,"filterNgramMinLength":3,"filterNgramMaxLength":5}}]}}}}]}`
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, tableModelFacet(c, "indexes", schema.IndexProperties, ngram))

	encoded, err := backend.New().EncodeSource(ctx, request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(indexKindParts(encoded), qt.DeepEquals, []string{
		`ft type="GLOBAL USING fulltext_relevance" columns=["body"] tokenizer="standard" use_filter_lowercase="true" use_filter_ngram="true" ` +
			`filter_ngram_min_length="3" filter_ngram_max_length="5"`,
	})
	c.Assert(decodedProjections(c, ctx, sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)), qt.DeepEquals, map[string]string{
		schema.IndexProperties + " app/records": string(request.Values[0].Value.CanonicalProjection),
	})
}

// A global index's partitioning by size turned off and its maximum partition
// count are written and read back in each format. Ported from the host's
// TestRender_IndexPartitioning.
func TestEncodeSource_WritesAnIndexsSizeSwitchAndMaximum(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			request := tableRequest(format, indexFacet(c, "by_body", `{"partitioningSettings":{"partitioningBySize":"DISABLED","minPartitionsCount":"2","maxPartitionsCount":"8"}}`))

			encoded, err := backend.New().EncodeSource(groupedEncodingContext(c), request)

			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(decodedProjections(c, groupedEncodingContext(c), sourceDocument(format, request, encoded)), qt.DeepEquals, map[string]string{
				schema.IndexSettingsProperties + " app/by_body": string(request.Values[0].Value.CanonicalProjection),
			})
		})
	}
}

// secretValue is the desired value of the secret name at the root, whose
// value variable is env.
func secretValue(c *qt.C, name, env string) *protocol.EncodeSourceValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Name: name}
	value, err := schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: env})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: "secret", Value: value,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(schema.SecretProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// A secret's directive stands after the topics, with them after the views,
// and before the nodes a host places after the default privileges. Ported
// from the host's TestEncodeAnnotations_WritesEachGroupWhereTheSourceDoes.
func TestEncodeSource_PlacesASecretAfterTheTopics(t *testing.T) {
	c := testcheck.New(t)
	features := []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.EncodeGroupsFeature, provider.GoPlacementsFeature}
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, node(c, "node", "", "locks", schema.CoordinationIntent{}), secretValue(c, "token", "PTAH_SECRET_TOKEN"),
		settledTopic(c))

	encoded, err := backend.New().EncodeSource(provider.ContextWithHostFeatures(c.Context(), features), request)

	c.Assert(err, qt.IsNil)
	var directives []string
	for _, line := range goLines(encoded) {
		directives = append(directives, line.Directive+" "+line.Placement.String())
	}
	c.Assert(directives, qt.DeepEquals, []string{
		"ptah:schema:topic PLACEMENT_UNSPECIFIED", "ptah:schema:topic:consumer PLACEMENT_UNSPECIFIED", "ptah:schema:secret PLACEMENT_UNSPECIFIED",
		"ptah:schema:coordinationnode GO_AFTER_PRIVILEGES",
	})
}

// An HCL export of a read that could not read a streaming query writes no
// loss and no limit for it: HCL declares no streaming query and claims
// nothing about them. Ported from the host's TestRender_LeavesOtherKindsLimitsAlone.
func TestEncodeSource_HCLLeavesAnUnreadStreamingQueryAlone(t *testing.T) {
	c := testcheck.New(t)
	query := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Name: "copy"}
	request := encodeRequest(protocol.SourceDocument_HCL)
	request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED,
		Kinds: []*protocol.ModelKindCoverage{kindKnowledge(schema.NativeDesiredModel(schema.StreamingQueryProperties), protocol.ModelKnowledge_COMPLETE)},
		Subjects: []*protocol.ModelSubjectCoverage{{Kind: schema.StreamingQueryProperties, Subject: query, NormalizedSubject: proto.CloneOf(query),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: "the read could not decode it"}}}}

	encoded, err := backend.New().EncodeSource(limitsContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.GetLosses(), qt.HasLen, 0)
	c.Assert(encoded.GetLimits(), qt.HasLen, 0)
}

// A Go export of a read that listed a topic or a secret and left it unread
// writes a limit naming that object, and reading the export back leaves only
// that object unmanaged. Ported from the host's
// TestGoExport_CarriesATopicAReadLeftUnread and
// TestGoExport_CarriesASecretAReadLeftUnmanaged.
func TestEncodeSource_CarriesAnObjectAReadLeftUnread(t *testing.T) {
	for _, test := range []struct {
		name, kind, directory, leaf, want string
	}{
		{name: "a topic", kind: schema.TopicProperties, directory: "app", leaf: "events", want: "topic app/events"},
		{name: "a secret", kind: schema.SecretProperties, directory: "ext", leaf: "pg.pw", want: "secret ext/pg.pw"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: test.kind, Schema: test.directory, Name: test.leaf}
			request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, node(c, "queue", "app", "queue", schema.CoordinationIntent{}))
			request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED,
				Kinds: []*protocol.ModelKindCoverage{kindKnowledge(schema.NativeDesiredModel(test.kind), protocol.ModelKnowledge_COMPLETE)},
				Subjects: []*protocol.ModelSubjectCoverage{{Kind: test.kind, Subject: owner, NormalizedSubject: proto.CloneOf(owner),
					Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "the target lacks the capability, so the read left it unread"}}}}
			encoded, err := backend.New().EncodeSource(limitsContext(c), request)
			c.Assert(err, qt.IsNil)
			c.Assert(limitTexts(encoded.GetLimits()), qt.DeepEquals, []string{test.want})
			document := sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)
			document.Documents[0].Limits = encoded.GetLimits()

			decoded, err := backend.New().DecodeSource(limitsContext(c), document)

			c.Assert(err, qt.IsNil)
			knowledge := decodedKnowledge(decoded.GetDocuments()[0].GetCoverage(), test.kind)
			c.Assert(knowledge, qt.Contains, test.kind+" COMPLETE")
			c.Assert(knowledge, qt.Contains, test.kind+" "+test.directory+"/"+test.leaf+" UNINSPECTED")
		})
	}
}

// A coordination node YDB would not keep as declared, a self-check period it
// moves into its range, is refused when its desired value is captured, so no
// export writes a block for it. Ported from the host's
// TestRenderRefusesACoordinationNodeYDBWouldNotKeep.
func TestEncodeSource_ANodeYDBWouldNotKeepIsNeverWritten(t *testing.T) {
	c := testcheck.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: "app", Name: "locks"}
	value, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{SelfCheckPeriodMillis: new(uint32(11_000))})
	c.Assert(err, qt.ErrorMatches, "YDB coordination periods would be clamped by the server")
	c.Assert(value, qt.IsNil)
}
