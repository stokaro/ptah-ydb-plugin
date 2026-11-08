package yqlschema_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ownedIndexKinds are the models that hold a table's vector, full-text and
// local indexes: the native index set of a row table and the storage of a
// column table.
var ownedIndexKinds = []string{schema.IndexProperties, schema.ColumnStorageProperties}

// ownedIndexFacets are the facets of ownedIndexKinds among envelopes.
func ownedIndexFacets(envelopes []*protocol.Envelope) []declaredFacet {
	return slices.DeleteFunc(declaredFacetsOf(envelopes), func(facet declaredFacet) bool { return !slices.Contains(ownedIndexKinds, facet.Kind) })
}

// goOwnedIndex decodes a Go source that declares app/items with the table
// directive's attributes and one index directive, for a host that negotiates
// source.index-kinds, and returns its facets of ownedIndexKinds.
func goOwnedIndex(c *qt.C, table, index map[string]string) []declaredFacet {
	c.Helper()
	request, err := testsource.GoDirectives([]testsource.GoTable{{Schema: "app", Name: "items", Struct: "Item"}}, []testsource.GoDeclaration{
		{Directive: "ptah:schema:table", Attributes: table, Struct: "Item", File: "entities.go", FileSize: 1000,
			Start: testsource.GoPosition{Offset: 0, Line: 1, Column: 1}, End: testsource.GoPosition{Offset: 40, Line: 1, Column: 41}},
		{Directive: "ptah:schema:index", Attributes: index, Struct: "Item", File: "entities.go", FileSize: 1000, Subject: testsource.GoSubject{Kind: "index", Name: index["name"]},
			Start: testsource.GoPosition{Offset: 100, Line: 2, Column: 1}, End: testsource.GoPosition{Offset: 140, Line: 2, Column: 41}},
	})
	c.Assert(err, qt.IsNil)
	ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.IndexKindsFeature})
	decoded, err := backend.New().DecodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	var envelopes []*protocol.Envelope
	for _, fragment := range decoded.GetDocuments()[0].GetFragments() {
		for _, value := range fragment.GetValues() {
			envelopes = append(envelopes, value.GetValue().GetValue())
		}
	}
	return ownedIndexFacets(envelopes)
}

// The vector, full-text and local indexes a YQL CREATE TABLE declares reach
// the reply as the facets a Go source's index directives of the same indexes
// declare, byte for byte, for a host that takes the facets of a reading and
// negotiates source.index-kinds: a vector or full-text index in the native
// index set of its row table, a local index in the storage of its column
// table. No common index stands for them, and the reading claims the native
// index set, so a table that declares none has none. The built-in YDB engine
// at host 74de0b65b read each of these and rendered it back (the host's
// TestParseRender).
func TestReply_OwnedIndexesAgreeWithGo_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name         string
		yql          string
		table, index map[string]string
		kind         string
	}{
		{name: "a vector index",
			yql: "CREATE TABLE `app/items` (id Uint64 NOT NULL, v String, PRIMARY KEY (id), INDEX i GLOBAL SYNC USING vector_kmeans_tree ON (v) " +
				"WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2));",
			table: make(map[string]string),
			index: map[string]string{"name": "i", "fields": "v", "type": "vector_kmeans_tree", "distance": "cosine", "vector_type": "float",
				"vector_dimension": "3", "levels": "1", "clusters": "2"},
			kind: schema.IndexProperties},
		{name: "a full-text index that covers a column",
			yql: "CREATE TABLE `app/items` (id Uint64 NOT NULL, v Utf8, w Utf8, PRIMARY KEY (id), INDEX i GLOBAL USING fulltext_plain ON (v) COVER (w) " +
				"WITH (tokenizer = 'standard', use_filter_lowercase = true));",
			table: make(map[string]string),
			index: map[string]string{"name": "i", "fields": "v", "include": "w", "type": "fulltext_plain", "tokenizer": "standard", "use_filter_lowercase": "true"},
			kind:  schema.IndexProperties},
		{name: "a local index of a column table",
			yql: "CREATE TABLE `app/items` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id), INDEX i LOCAL USING bloom_filter ON (v) " +
				"WITH (false_positive_probability = 0.05)) PARTITION BY HASH (id) WITH (STORE = COLUMN);",
			table: map[string]string{"store": "COLUMN", "partition_by_hash": "id"},
			index: map[string]string{"name": "i", "fields": "v", "type": "bloom_filter", "false_positive_probability": "0.05"},
			kind:  schema.ColumnStorageProperties},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := schemaRequest(backend.Description().GetEngines()[0].GetFeatures(), test.yql)

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
			c.Assert(reply.GetTables(), qt.HasLen, 1)
			c.Assert(reply.GetTables()[0].GetIndexes(), qt.HasLen, 0)
			read := ownedIndexFacets(reply.GetFacets())
			c.Assert(read, qt.HasLen, 1)
			c.Assert(read[0].Kind, qt.Equals, test.kind)
			c.Assert(read, qt.DeepEquals, goOwnedIndex(c, test.table, test.index))
			c.Assert(claimedKinds(reply), qt.Contains, schema.IndexProperties)
		})
	}
}

// What a table's vector, full-text and local indexes cannot carry is refused
// by name, with no schema: a host that does not take them has no home for
// any of them, and an index on a table of the other storage, a comment on
// one and a vector index's partitioning are refused with the words a YAML or
// Go source of them is refused with, or with what the native model lacks.
func TestReply_OwnedIndexes_FailurePath(t *testing.T) {
	const table = "CREATE TABLE `app/items` (id Uint64 NOT NULL, v String, PRIMARY KEY (id), "
	for _, test := range []struct {
		name, without, text, want string
	}{
		{name: "a host without source.index-kinds", without: provider.IndexKindsFeature,
			text: table + "INDEX i GLOBAL USING vector_kmeans_tree ON (v) WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2));",
			want: "index `i` of table `app/items` is a vector_kmeans_tree index, which a SQL schema reading has no home for: the host does not take " +
				"a table's vector, full-text and local indexes in a SQL schema reading (schema.sql-facets and source.index-kinds)"},
		{name: "a local index of a row table",
			text: table + "INDEX i LOCAL USING bloom_filter ON (v));",
			want: "table `app/items`: index `i`: a local index belongs to a column table, and the table is declared as a row table"},
		{name: "a vector index of a column table",
			text: table + "INDEX i GLOBAL USING vector_kmeans_tree ON (v) WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2)) " +
				"PARTITION BY HASH (id) WITH (STORE = COLUMN);",
			want: "table `app/items`: index `i`: a column table takes local indexes only; a vector_kmeans_tree index belongs to a row table"},
		{name: "a vector index's partitioning",
			text: table + "INDEX i GLOBAL USING vector_kmeans_tree ON (v) WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2, " +
				"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2));",
			want: "table `app/items`: index `i`: a vector index states no partitioning this provider keeps"},
		{name: "a comment on a vector index",
			text: table + "INDEX i GLOBAL USING vector_kmeans_tree ON (v) WITH (distance = 'cosine', vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2));\n" +
				"COMMENT ON INDEX `i` ON `app/items` IS 'nearest';",
			want: "table `app/items`: index `i`: a comment on a vector, full-text or local index has no place this provider reads back"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := schemaRequest(withoutFeature(test.without), test.text)
			negotiated := provider.NegotiatedDescription(backend.Description(), slices.DeleteFunc(slices.Clone(provider.TwoWayFeatures()), func(feature string) bool {
				return feature == test.without
			}))

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(negotiated, request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			c.Assert(reply.GetTables(), qt.HasLen, 0)
			c.Assert(reply.GetFacets(), qt.HasLen, 0)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Equals, test.want)
		})
	}
}
