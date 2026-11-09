package tablesource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexDirective is a ptah:schema:index directive of the Item struct on its
// own line of the file, the line number being its position in the list.
func indexDirective(line int, attributes map[string]string) testsource.GoDeclaration {
	start := (line - 1) * 100
	return testsource.GoDeclaration{Directive: "ptah:schema:index", Attributes: attributes, Struct: "Item", File: "entities.go", FileSize: fileSize,
		Start: testsource.GoPosition{Offset: start, Line: line, Column: 1}, End: testsource.GoPosition{Offset: start + 40, Line: line, Column: 41},
		Subject: testsource.GoSubject{Kind: "index", Name: attributes["name"]}}
}

// settingsDirective is the provider's attributes of a ptah:schema:index
// directive of another type than the provider's index kinds, which the host
// sends alone, with the index as the subject.
func settingsDirective(line int, name string, attributes map[string]string) testsource.GoDeclaration {
	declaration := indexDirective(line, attributes)
	declaration.Subject.Name = name
	return declaration
}

// columnTableDirective declares the Item struct's table a column table hashed
// by id in one shard.
func columnTableDirective() testsource.GoDeclaration {
	return testsource.GoDeclaration{Directive: "ptah:schema:table", Attributes: map[string]string{"store": "COLUMN", "partition_by_hash": "id", "column_shards": "1"},
		Struct: "Item", File: "entities.go", FileSize: fileSize, Start: testsource.GoPosition{Line: 1, Column: 1}, End: testsource.GoPosition{Offset: 40, Line: 1, Column: 41}}
}

// decodedFacets decodes a Go file through the provider with host features in
// the context, and returns the kind and payload of each value it decodes, in
// the order of the fragments.
func decodedFacets(c *qt.C, features []string, declarations ...testsource.GoDeclaration) (kinds, payloads []string) {
	c.Helper()
	request := goRequest(c, declarations...)
	decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), features), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
	for _, fragment := range decoded.Documents[0].Fragments {
		for _, value := range fragment.Values {
			kinds = append(kinds, value.Value.Value.Kind)
			payloads = append(payloads, string(value.Value.Value.Payload))
		}
	}
	return kinds, payloads
}

// An index directive of a vector, full-text or local type reaches the
// provider whole and declares the table's native index set or its column
// storage, with the settings the host's embedded engine read from it.
func TestIndexKindDirectives_HappyPath(t *testing.T) {
	vector := map[string]string{"name": "docs_emb", "fields": "gen, emb", "include": "body", "type": "vector_kmeans_tree", "distance": "cosine",
		"vector_type": "float", "vector_dimension": "3", "levels": "1", "clusters": "2"}
	tests := []struct {
		name         string
		features     []string
		declarations []testsource.GoDeclaration
		wantKinds    []string
		wantPayloads []string
	}{
		{"a vector index with its settings", nil, []testsource.GoDeclaration{indexDirective(2, vector)}, []string{schema.IndexProperties},
			[]string{`{"indexes":[{"specification":{"dataColumns":["body"],"globalVectorKmeansTreeIndex":{"vectorSettings":{"clusters":2,"levels":1,` +
				`"settings":{"metric":"DISTANCE_COSINE","vectorDimension":3,"vectorType":"VECTOR_TYPE_FLOAT"}}},"indexColumns":["gen","emb"],"name":"docs_emb"}}]}`}},
		{"a vector index naming its metric by a pgvector class", nil, []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "docs_ip",
			"fields": "emb", "type": "GLOBAL USING vector_kmeans_tree", "ops": "vector_ip_ops", "vector_type": "int8", "vector_dimension": "3", "levels": "2", "clusters": "2"})},
			[]string{schema.IndexProperties},
			[]string{`{"indexes":[{"specification":{"globalVectorKmeansTreeIndex":{"vectorSettings":{"clusters":2,"levels":2,` +
				`"settings":{"metric":"SIMILARITY_INNER_PRODUCT","vectorDimension":3,"vectorType":"VECTOR_TYPE_INT8"}}},"indexColumns":["emb"],"name":"docs_ip"}}]}`}},
		{"a vector index without settings, which the planner refuses by name", nil, []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "docs_emb",
			"fields": "emb", "type": "vector_kmeans_tree"})}, []string{schema.IndexProperties},
			[]string{`{"indexes":[{"specification":{"globalVectorKmeansTreeIndex":{"vectorSettings":{}},"indexColumns":["emb"],"name":"docs_emb"}}]}`}},
		{"two indexes of one table in one facet", nil, []testsource.GoDeclaration{indexDirective(2, vector), indexDirective(3, map[string]string{"name": "ft",
			"fields": "body", "type": "fulltext_relevance", "tokenizer": "standard", "use_filter_lowercase": "true"})}, []string{schema.IndexProperties},
			[]string{`{"indexes":[{"specification":{"dataColumns":["body"],"globalVectorKmeansTreeIndex":{"vectorSettings":{"clusters":2,"levels":1,` +
				`"settings":{"metric":"DISTANCE_COSINE","vectorDimension":3,"vectorType":"VECTOR_TYPE_FLOAT"}}},"indexColumns":["gen","emb"],"name":"docs_emb"}},` +
				`{"specification":{"globalFulltextRelevanceIndex":{"fulltextSettings":{"columns":[{"analyzers":{"tokenizer":"STANDARD","useFilterLowercase":true},` +
				`"column":"body"}]}},"indexColumns":["body"],"name":"ft"}}]}`}},
		{"a plain full-text index", nil, []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain",
			"tokenizer": "keyword", "use_filter_stopwords": "false"})}, []string{schema.IndexProperties},
			[]string{`{"indexes":[{"specification":{"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[{"analyzers":{"tokenizer":"KEYWORD","useFilterStopwords":false},` +
				`"column":"body"}]}},"indexColumns":["body"],"name":"ft"}}]}`}},
		{"local indexes of a column table, the storage stating them all", []string{provider.IndexKindsFeature},
			[]testsource.GoDeclaration{columnTableDirective(), indexDirective(2, map[string]string{"name": "bf", "fields": "body", "type": "bloom_filter",
				"false_positive_probability": "0.01"}), indexDirective(3, map[string]string{"name": "ng", "fields": "body", "type": "LOCAL USING bloom_ngram_filter",
				"ngram_size": "4", "case_sensitive": "false"}), indexDirective(4, map[string]string{"name": "mm", "fields": "id", "type": "min_max"})},
			[]string{schema.ColumnStorageProperties},
			[]string{`{"hashColumns":["id"],"localIndexes":[{"bloom":{"falsePositiveProbability":0.01},"columns":["body"],"name":"bf"},` +
				`{"columns":["id"],"minMax":{},"name":"mm"},{"columns":["body"],"name":"ng","ngram":{"caseSensitive":false,"ngramSize":"4"}}],"localIndexesStated":true,"partitions":"1"}`}},
		{"a column table of a host that does not negotiate index kinds", nil, []testsource.GoDeclaration{columnTableDirective()},
			[]string{schema.ColumnStorageProperties}, []string{`{"hashColumns":["id"],"partitions":"1"}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			kinds, payloads := decodedFacets(c, test.features, test.declarations...)
			c.Assert(kinds, qt.DeepEquals, test.wantKinds)
			c.Assert(payloads, qt.DeepEquals, test.wantPayloads)
		})
	}
}

func TestIndexKindDirectives_FailurePath(t *testing.T) {
	tests := []struct {
		name         string
		declarations []testsource.GoDeclaration
		want         string
	}{
		{"a unique vector index", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "unique": "true"})},
			`table "items": index "v": a YDB vector_kmeans_tree index is never unique`},
		{"a comment", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "comment": "nearest"})},
			`table "items": index "v": a comment on a vector, full-text or local index has no place this provider reads back`},
		{"a vector setting on a full-text index", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain",
			"tokenizer": "standard", "levels": "2"})}, `table "items": index "ft": levels is not a setting of a fulltext_plain index`},
		{"an operator class on a full-text index", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain",
			"tokenizer": "standard", "ops": "gin_trgm_ops"})}, `table "items": index "ft": an operator class names the metric of a vector index, and a fulltext_plain index takes none`},
		{"a class with no YDB metric", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "ops": "vector_hamming_ops"})},
			`table "items": index "v": operator class "vector_hamming_ops" has no YDB counterpart: .*`},
		// Ported from the host's TestRender_VectorIndex_FailurePath: pgvector's
		// half-precision class has no YDB counterpart.
		{"a half-precision class", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", "ops": "halfvec_cosine_ops"})},
			`table "items": index "v": operator class "halfvec_cosine_ops" has no YDB counterpart: .*`},
		{"a class against the settings", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree",
			"ops": "vector_ip_ops", "distance": "cosine"})}, `table "items": index "v": operator class "vector_ip_ops" names similarity=inner_product, and the index's settings name distance=cosine`},
		{"a full-text index without a tokenizer", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain",
			"use_filter_lowercase": "true"})}, `table "items": index "ft": a YDB full-text index requires tokenizer`},
		{"a probability out of range", []testsource.GoDeclaration{columnTableDirective(), indexDirective(2, map[string]string{"name": "bf", "fields": "body",
			"type": "bloom_filter", "false_positive_probability": "1.5"})}, `table "items": index "bf": false_positive_probability must be between 0 and 1, exclusively`},
		{"an n-gram setting on a bloom index", []testsource.GoDeclaration{columnTableDirective(), indexDirective(2, map[string]string{"name": "bf", "fields": "body",
			"type": "bloom_filter", "ngram_size": "3"})}, `table "items": index "bf": ngram_size is not a setting of a bloom_filter index`},
		{"a local index of a row table", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "bf", "fields": "body", "type": "bloom_filter"})},
			`table "items": index "bf": a local index belongs to a column table, and the table is declared as a row table`},
		{"a vector index of a column table", []testsource.GoDeclaration{columnTableDirective(), indexDirective(2, map[string]string{"name": "v", "fields": "emb",
			"type": "vector_kmeans_tree"})}, `table "items": index "v": a column table takes local indexes only; a vector_kmeans_tree index belongs to a row table`},
		{"an index declared twice", []testsource.GoDeclaration{indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree"}),
			indexDirective(3, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree"})}, `table "items": index "v" is declared twice`},
		{"a vector setting on a sync index", []testsource.GoDeclaration{settingsDirective(2, "g", map[string]string{"distance": "cosine"})},
			`distance is not supported by the YDB provider yet: .*declared on an index of that type.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := goRequest(c, test.declarations...)
			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.IndexKindsFeature}), request)
			c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = `+test.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
}

// A setting value an index cannot take reaches the host as a reason it
// prints, naming the table, the index and what the setting takes, without the
// value the declaration wrote: the host printed only `provider decode source
// failed (InvalidArgument)`, where its embedded engine named the index and
// the setting. Ported from the host's TestParse_YDBVectorIndex_FailurePath
// (internal/atlashcl and internal/ydbsource), for the words the host prints.
func TestIndexKindsGiveTheReasonOfASettingValue(t *testing.T) {
	for _, test := range []struct {
		name, setting, value, want string
	}{
		{name: "a distance", setting: "distance", value: "inner_product",
			want: `table "items": index "v": invalid distance: write one of cosine, euclidean, manhattan`},
		{name: "a similarity", setting: "similarity", value: "euclidean",
			want: `table "items": index "v": invalid similarity: write one of inner_product, cosine`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := goRequest(c, indexDirective(2, map[string]string{"name": "v", "fields": "emb", "type": "vector_kmeans_tree", test.setting: test.value}))

			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.IndexKindsFeature}), request)

			c.Assert(decoded, qt.IsNil)
			details := status.Convert(err).Details()
			c.Assert(details, qt.HasLen, 1)
			c.Assert(details[0], qt.CmpEquals(protocmp.Transform()), &protocol.ProviderDiagnostic{Reason: test.want})
		})
	}
}

// An HCL document declares no local index: HCL cannot state the column table
// it belongs to.
func TestIndexKindsRefuseALocalIndexInHCL_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := goRequest(c, indexDirective(2, map[string]string{"name": "bf", "fields": "body", "type": "bloom_filter"}))
	document := request.Documents[0]
	document.Format, document.File = protocol.SourceDocument_HCL, "schema.hcl"
	document.Models = []*protocol.ValueModel{schema.NativeDesiredModel(schema.ColumnStorageProperties)}
	document.Fragments[0].GetValue().GetMapping().Entries = nil

	decoded, err := backend.New().DecodeSource(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = table "items": index "bf": an HCL document declares no local index, `+
		`since HCL cannot state the storage of the column table it belongs to`)
	c.Assert(decoded, qt.IsNil)
}

// A YAML index entry carries its settings as YAML values, a count as a number
// and a switch as a boolean, which read as the text a Go directive writes.
func TestIndexKindEntriesOfYAML_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := goRequest(c, indexDirective(2, map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain"}))
	document := request.Documents[0]
	document.Format, document.File = protocol.SourceDocument_YAML, "schema.yaml"
	document.Models = []*protocol.ValueModel{schema.NativeDesiredModel(schema.IndexProperties)}
	span := document.Fragments[0].Span
	entry := func(key string, value *protocol.SourceValue) *protocol.SourceEntry {
		value.Span = span
		return &protocol.SourceEntry{Key: key, KeySpan: span, Value: value}
	}
	document.Fragments[0].GetValue().GetMapping().Entries = []*protocol.SourceEntry{
		entry("tokenizer", &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: "standard"}}),
		entry("use_filter_lowercase", &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: true}}),
		entry("use_filter_length", &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: true}}),
		entry("filter_length_min", &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: "2"}}),
	}

	decoded, err := backend.New().DecodeSource(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
	c.Assert(string(decoded.Documents[0].Fragments[0].Values[0].Value.Value.Payload), qt.Equals,
		`{"indexes":[{"specification":{"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[{"analyzers":{"filterLengthMin":2,"tokenizer":"STANDARD",`+
			`"useFilterLength":true,"useFilterLowercase":true},"column":"body"}]}},"indexColumns":["body"],"name":"ft"}}]}`)
}
