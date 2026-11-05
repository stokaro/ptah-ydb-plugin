package backend_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// tableModelFacet is the desired facet of kind of app/records with payload.
func tableModelFacet(c *qt.C, id, kind, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	model := schema.NativeDesiredModel(kind)
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: table, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: id,
		Value: envelope, NormalizedOwner: proto.CloneOf(table), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

// indexKindsContext is the context of a call from a host that writes encoding
// groups, reads the provider's table platform group and its index kinds.
func indexKindsContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.HCLPlatformFeature,
		provider.EncodeGroupsFeature, provider.IndexKindsFeature})
}

const (
	// vectorAndText is a native index set as a source declares it: a vector
	// index over a prefix that covers a column, and a full-text index.
	vectorAndText = `{"indexes":[{"specification":{"name":"docs_emb","indexColumns":["gen","emb"],"dataColumns":["body"],"globalVectorKmeansTreeIndex":{` +
		`"vectorSettings":{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":2}}}},` +
		`{"specification":{"name":"ft","indexColumns":["body"],"globalFulltextRelevanceIndex":{"fulltextSettings":{"columns":[{"column":"body",` +
		`"analyzers":{"tokenizer":"STANDARD","useFilterLowercase":true}}]}}}}]}`
	// readVector is a vector index as a read converts it, with the tables
	// that implement it and the settings YDB gives each.
	readVector = `{"indexes":[{"specification":{"name":"docs_emb","indexColumns":["emb"],"globalVectorKmeansTreeIndex":{` +
		`"levelTableSettings":{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}},` +
		`"vectorSettings":{"settings":{"metric":"SIMILARITY_INNER_PRODUCT","vectorType":"VECTOR_TYPE_INT8","vectorDimension":3},"levels":2,"clusters":2}}},` +
		`"implementation":[{"name":"indexImplLevelTable","settings":{"primaryKey":["__ydb_id"]},"columns":[{"name":"__ydb_id","type":{"typeId":"UINT64"},"settings":{}}]},` +
		`{"name":"indexImplPostingTable","settings":{"primaryKey":["__ydb_parent","id"]},"columns":[{"name":"__ydb_parent","type":{"typeId":"UINT64"},"settings":{}},` +
		`{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}]}]}`
	// localIndexes is a column table's storage with a bloom and an n-gram
	// local index.
	localIndexes = `{"hashColumns":["id"],"partitions":"1","localIndexes":[{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.01}},` +
		`{"name":"ng","columns":["body"],"ngram":{"ngramSize":"3","caseSensitive":true}}]}`
)

// indexKindParts are the inherited attributes and the content of each part
// with an index-kind claim, spelled as one line each.
func indexKindParts(encoded *protocol.EncodedSource) []string {
	var parts []string
	for _, part := range provider.EncodedSourceParts(encoded) {
		fragment := part.GetFragment()
		if fragment.GetIndexKind() == nil {
			continue
		}
		written := []string{fragment.GetSubject().GetName()}
		for _, entry := range append(fragment.GetInheritedAttributes().GetEntries(), fragment.GetValue().GetMapping().GetEntries()...) {
			written = append(written, entry.GetKey()+"="+spelledSource(entry.GetValue()))
		}
		parts = append(parts, strings.Join(written, " "))
	}
	return parts
}

// spelledSource writes a value as a line: text quoted, a number bare, a
// sequence in brackets.
func spelledSource(value *protocol.SourceValue) string {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return `"` + content.Text + `"`
	case *protocol.SourceValue_Number:
		return content.Number
	case *protocol.SourceValue_Sequence:
		items := make([]string, 0, len(content.Sequence.GetValues()))
		for _, item := range content.Sequence.GetValues() {
			items = append(items, spelledSource(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return "?"
}

// Go writes the vector and full-text indexes of a table's native index set as
// index directives of their types, which decode back to the set, and a column
// table's local indexes beside its table directive.
func TestEncodeSource_WritesIndexKinds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	ctx := indexKindsContext(c)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, tableModelFacet(c, "indexes", schema.IndexProperties, vectorAndText))
	encoded, err := backend.New().EncodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(ctx)), request, encoded), qt.IsNil)
	c.Assert(indexKindParts(encoded), qt.DeepEquals, []string{
		`docs_emb type="GLOBAL USING vector_kmeans_tree" columns=["gen", "emb"] include=["body"] distance="cosine" vector_type="float" vector_dimension="3" levels="1" clusters="2"`,
		`ft type="GLOBAL USING fulltext_relevance" columns=["body"] tokenizer="standard" use_filter_lowercase="true"`,
	})
	c.Assert(decodedProjections(c, ctx, sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)), qt.DeepEquals, map[string]string{
		schema.IndexProperties + " app/records": string(request.Values[0].Value.CanonicalProjection),
	})

	local := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, tableModelFacet(c, "storage", schema.ColumnStorageProperties, localIndexes))
	encoded, err = backend.New().EncodeSource(ctx, local)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(indexKindParts(encoded), qt.DeepEquals, []string{
		`bf type="bloom_filter" columns=["body"] false_positive_probability="0.01"`,
		`ng type="bloom_ngram_filter" columns=["body"] ngram_size="3" case_sensitive="true"`,
	})
	c.Assert(decodedProjections(c, ctx, sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, local, encoded)), qt.DeepEquals, map[string]string{
		schema.ColumnStorageProperties + " app/records": `{"hashColumns":["id"],"localIndexes":[{"bloom":{"falsePositiveProbability":0.01},"columns":["body"],"name":"bf"},` +
			`{"columns":["body"],"name":"ng","ngram":{"caseSensitive":true,"ngramSize":"3"}}],"localIndexesStated":true,"partitions":"1"}`,
	})
}

// HCL writes a vector index as the host's embedded engine wrote its index
// block: its type GLOBAL USING vector_kmeans_tree, the column it covers, and
// its settings, a count as a number. A read's index, with the tables that
// implement it, is written as the index it declares.
func TestEncodeSource_WritesIndexKindsInHCL(t *testing.T) {
	c := testcheck.New(t)
	ctx := indexKindsContext(c)
	request := tableRequest(protocol.SourceDocument_HCL, tableModelFacet(c, "indexes", schema.IndexProperties, vectorAndText))
	encoded, err := backend.New().EncodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(provider.ValidateEncodedSource(provider.NegotiatedDescription(backend.Description(), provider.HostFeatures(ctx)), request, encoded), qt.IsNil)
	c.Assert(indexKindParts(encoded), qt.DeepEquals, []string{
		`docs_emb type="GLOBAL USING vector_kmeans_tree" columns=["gen", "emb"] include=["body"] distance="cosine" vector_type="float" vector_dimension=3 levels=1 clusters=2`,
		`ft type="GLOBAL USING fulltext_relevance" columns=["body"] tokenizer="standard" use_filter_lowercase="true"`,
	})

	read := tableRequest(protocol.SourceDocument_HCL, tableModelFacet(c, "indexes", schema.IndexProperties, readVector))
	encoded, err = backend.New().EncodeSource(ctx, read)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(indexKindParts(encoded), qt.DeepEquals, []string{
		`docs_emb type="GLOBAL USING vector_kmeans_tree" columns=["emb"] similarity="inner_product" vector_type="int8" vector_dimension=3 levels=2 clusters=2`,
	})
}

// Without the feature the native indexes and the local indexes are losses, as
// before the host could read their declarations; HCL loses a column table's
// local indexes with its storage, which it cannot state.
func TestEncodeSource_LosesIndexKinds(t *testing.T) {
	for _, test := range []struct {
		name          string
		format        protocol.SourceDocument_Format
		features      []string
		kind, payload string
		want          string
	}{
		{"native indexes of a host without index kinds", protocol.SourceDocument_GO_ANNOTATIONS, []string{provider.EncodeGroupsFeature},
			schema.IndexProperties, vectorAndText, `table app/records: its native indexes (docs_emb, ft) have no source spelling the provider writes`},
		{"local indexes of a host without index kinds", protocol.SourceDocument_GO_ANNOTATIONS, []string{provider.EncodeGroupsFeature},
			schema.ColumnStorageProperties, localIndexes, `table app/records: its YDB settings cannot be written so that a source reads them back unchanged`},
		{"local indexes in HCL", protocol.SourceDocument_HCL, []string{provider.EncodeGroupsFeature, provider.HCLSourceFeature, provider.HCLPlatformFeature, provider.IndexKindsFeature},
			schema.ColumnStorageProperties, localIndexes,
			`YDB column storage, hash partitioning and tiered TTL are not represented in HCL; local indexes bf, ng are not represented in HCL`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			ctx := provider.ContextWithHostFeatures(c.Context(), append([]string{provider.EntryKeysFeature}, test.features...))
			request := tableRequest(test.format, tableModelFacet(c, "facet", test.kind, test.payload))
			encoded, err := backend.New().EncodeSource(ctx, request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 1)
			c.Assert(encoded.Losses[0].Message, qt.Equals, test.want)
		})
	}
}
