package tablecreate_test

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The tables YDB implements each kind of vector and full-text index with.
var (
	vectorTables    = []string{"indexImplLevelTable", "indexImplPostingTable"}
	prefixedTables  = []string{"indexImplLevelTable", "indexImplPostingTable", "indexImplPrefixTable"}
	plainTables     = []string{"indexImplTable"}
	relevanceTables = []string{"indexImplDictTable", "indexImplDocsTable", "indexImplStatsTable", "indexImplTable"}
)

// implementationDefaults is the partitioning YDB reports for each table of a
// new vector or full-text index, measured on 25.1.4.7 and 26.2.1.14.
const implementationDefaults = `{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}}`

// kmeans is the settings of a vector index.
func kmeans(metric, vectorType string, dimension, levels, clusters int) string {
	return fmt.Sprintf(`{"settings":{"metric":%q,"vectorType":%q,"vectorDimension":%d},"levels":%d,"clusters":%d}`, metric, vectorType, dimension, levels, clusters)
}

// cosine3 is a vector index over float vectors of three elements, compared
// by cosine distance, with clusters clusters on one level.
func cosine3(clusters int) string {
	return kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 3, 1, clusters)
}

// vectorJSON states one vector index; extra is added to its kind, such as the
// settings of the tables that implement it.
func vectorJSON(name string, keys, cover []string, settings, extra string) string {
	text := `"name":"` + name + `","indexColumns":` + quoted(keys...) + `,"globalVectorKmeansTreeIndex":{"vectorSettings":` + settings + extra + `}`
	if len(cover) > 0 {
		text += `,"dataColumns":` + quoted(cover...)
	}
	return text
}

// fullTextJSON states one full-text index of kind, globalFulltextPlainIndex
// or globalFulltextRelevanceIndex, on column with its analyzers.
func fullTextJSON(kind, name, column, analyzers string) string {
	return `"name":"` + name + `","indexColumns":["` + column + `"],"` + kind + `":{"fulltextSettings":{"columns":[{"column":"` + column + `","analyzers":` + analyzers + `}]}}`
}

// nativeDeclared is one declaration of an index set, with the requirement on
// each table that implements it the model asks for.
func nativeDeclared(spec string, tables []string) string {
	requirements := make([]string, len(tables))
	for i, table := range tables {
		requirements[i] = `{"name":"` + table + `","settings":{"primaryKey":["id"]},"columns":[{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}`
	}
	return `{"specification":{` + spec + `},"implementation":[` + strings.Join(requirements, ",") + `]}`
}

// nativeHeld is one index a table holds, ready, with each table that
// implements it.
func nativeHeld(description string, tables []string) string {
	implementations := make([]string, len(tables))
	for i, table := range tables {
		implementations[i] = `{"name":"` + table + `","description":{"columns":[{"name":"id","type":{"typeId":"UINT64"}}],"primaryKey":["id"]}}`
	}
	return `{"description":{` + description + `,"status":"STATUS_READY"},"tables":[` + strings.Join(implementations, ",") + `]}`
}

func indexSet(entries ...string) string { return `{"indexes":[` + strings.Join(entries, ",") + `]}` }

// everyIndexKind is a target that builds every vector and full-text index.
func everyIndexKind() map[string]bool {
	return map[string]bool{tablecreate.VectorIndexesCapability: true, tablecreate.VectorBitTypeCapability: true, tablecreate.FullTextIndexesCapability: true}
}

// documents is a table of documents: a generation, a vector, a body, a title
// and a score, which the declaration and the table both hold.
func documents(desired, held string) alterCase {
	columns := []string{"gen", "emb", "body", "title", "score"}
	return alterCase{desiredTable: plainTable, heldTable: plainTable, declared: columns, held: columns,
		types:          map[string]string{"gen": "Int64", "emb": "String", "body": "Utf8", "title": "Utf8", "score": "Double"},
		desiredIndexes: desired, heldIndexes: held, capabilities: everyIndexKind()}
}

var (
	docsEmb = vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, cosine3(2), "")
	// docsEmbRead is docs_emb as the server reports it: with the partitioning
	// of each table that implements it.
	docsEmbRead = vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, cosine3(2),
		`,"levelTableSettings":`+implementationDefaults+`,"postingTableSettings":`+implementationDefaults+`,"prefixTableSettings":`+implementationDefaults)
	standardBody  = fullTextJSON("globalFulltextRelevanceIndex", "ft", "body", `{"tokenizer":"STANDARD","useFilterLowercase":true}`)
	keywordBody   = fullTextJSON("globalFulltextRelevanceIndex", "ft", "body", `{"tokenizer":"KEYWORD","useFilterLowercase":true}`)
	plainKeyword  = fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"KEYWORD","useFilterLowercase":true,"useFilterStopwords":false}`)
	addDocsEmbSQL = "ALTER TABLE `app/records` ADD INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) " +
		"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2);"
)

// plannedSteps is each step of a plan by its name and operation kind.
func plannedSteps(plan *protocol.PlannedFeatures) []string {
	var result []string
	for _, contribution := range plan.Contributions {
		for _, step := range contribution.Steps {
			result = append(result, step.Id.Name+" "+strings.TrimPrefix(step.Operation.Value.Value.Kind, schema.ProviderID+"/"))
		}
	}
	return result
}

// textUnits renders a plan in the TEXT form and returns its statements.
func textUnits(c *qt.C, plan *protocol.PlannedFeatures, capabilities map[string]bool) []string {
	c.Helper()
	rendering := testplanning.RenderRequest(backend.Description(), plan, capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	var result []string
	for _, unit := range rendered.Units {
		result = append(result, unit.GetSql())
	}
	return result
}

// Each change of a vector or full-text index the planner makes, with the
// steps, the order and the statements it takes. YDB changes nothing of such an
// index in place, so a change of its settings, its kind or its name is a
// removal and an addition, the removal first, as the host's embedded engine
// plans it.
func TestNativeIndexPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		in    alterCase
		steps []string
		edges []string
		sql   []string
	}{
		{"adds a vector index over a prefix that covers a column", documents(indexSet(nativeDeclared(docsEmb, prefixedTables)), indexSet()),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"}, nil, []string{addDocsEmbSQL}},
		{"adds a vector index named by similarity", documents(indexSet(nativeDeclared(vectorJSON("docs_title_ip", []string{"emb"}, nil,
			kmeans("SIMILARITY_INNER_PRODUCT", "VECTOR_TYPE_INT8", 3, 2, 2), ""), vectorTables)), indexSet()),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"}, nil,
			[]string{"ALTER TABLE `app/records` ADD INDEX `docs_title_ip` GLOBAL USING vector_kmeans_tree ON (`emb`) " +
				"WITH (similarity=inner_product, vector_type=int8, vector_dimension=3, levels=2, clusters=2);"}},
		{"adds a full-text index with its analyzers in name order", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "title",
			`{"tokenizer":"WHITESPACE","language":"english","useFilterSnowball":true,"useFilterStopwords":false}`), plainTables)), indexSet()),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"}, nil,
			[]string{"ALTER TABLE `app/records` ADD INDEX `ft` GLOBAL USING fulltext_plain ON (`title`) " +
				"WITH (language='english', tokenizer=whitespace, use_filter_snowball=true, use_filter_stopwords=false);"}},
		{"a changed setting drops the index and adds it again", documents(indexSet(nativeDeclared(vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, cosine3(4), ""), prefixedTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"row-table-drop-index/000000 -> row-table-add-index/000000"},
			[]string{"ALTER TABLE `app/records` DROP INDEX `docs_emb`;", strings.Replace(addDocsEmbSQL, "clusters=2", "clusters=4", 1)}},
		{"a renamed index is dropped and added under its new name", documents(indexSet(nativeDeclared(vectorJSON("docs_emb_cosine", []string{"gen", "emb"}, []string{"body"}, cosine3(2), ""), prefixedTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-native-index-operation"}, nil,
			[]string{"ALTER TABLE `app/records` DROP INDEX `docs_emb`;", strings.Replace(addDocsEmbSQL, "`docs_emb`", "`docs_emb_cosine`", 1)}},
		{"a vector index the declaration leaves out is dropped", documents(indexSet(), indexSet(nativeHeld(docsEmbRead, prefixedTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation"}, nil, []string{"ALTER TABLE `app/records` DROP INDEX `docs_emb`;"}},
		{"a vector index a source of index kinds states none for is dropped", statedNone(documents("", indexSet(nativeHeld(docsEmbRead, prefixedTables)))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation"}, nil, []string{"ALTER TABLE `app/records` DROP INDEX `docs_emb`;"}},
		// An index whose key the plan changes is built once, with the
		// declared vector settings. Ported from the host's
		// TestVectorIndexService_LeavesACommonReplacementAlone.
		{"another key drops the index and adds it again with its settings", documents(indexSet(nativeDeclared(vectorJSON("docs_emb", []string{"emb"}, []string{"body"}, cosine3(2), ""), vectorTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"row-table-drop-index/000000 -> row-table-add-index/000000"},
			[]string{"ALTER TABLE `app/records` DROP INDEX `docs_emb`;", strings.Replace(addDocsEmbSQL, "(`gen`, `emb`)", "(`emb`)", 1)}},
		{"another tokenizer rebuilds a full-text index", documents(indexSet(nativeDeclared(keywordBody, relevanceTables)), indexSet(nativeHeld(standardBody, relevanceTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"row-table-drop-index/000000 -> row-table-add-index/000000"},
			[]string{"ALTER TABLE `app/records` DROP INDEX `ft`;",
				"ALTER TABLE `app/records` ADD INDEX `ft` GLOBAL USING fulltext_relevance ON (`body`) WITH (tokenizer=keyword, use_filter_lowercase=true);"}},
		{"a relevance index declared plain is rebuilt", documents(indexSet(nativeDeclared(plainKeyword, plainTables)), indexSet(nativeHeld(keywordBody, relevanceTables))),
			[]string{"row-table-drop-index/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"row-table-drop-index/000000 -> row-table-add-index/000000"},
			[]string{"ALTER TABLE `app/records` DROP INDEX `ft`;",
				"ALTER TABLE `app/records` ADD INDEX `ft` GLOBAL USING fulltext_plain ON (`body`) WITH (tokenizer=keyword, use_filter_lowercase=true, use_filter_stopwords=false);"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, test.in)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plannedSteps(plan), qt.DeepEquals, test.steps)
			c.Assert(edges(plan), qt.DeepEquals, test.edges)
			c.Assert(textUnits(c, plan, request.Capabilities), qt.DeepEquals, test.sql)
		})
	}
}

// An index the table holds as declared plans nothing, whatever the server
// reports of the tables that implement it and whether a filter switched off
// is stated or left out.
func TestNativeIndexPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"a vector index read back with its implementation settings", documents(indexSet(nativeDeclared(docsEmb, prefixedTables)), indexSet(nativeHeld(docsEmbRead, prefixedTables)))},
		{"a vector index declared from a read of it", documents(indexSet(nativeDeclared(docsEmbRead, prefixedTables)), indexSet(nativeHeld(docsEmbRead, prefixedTables)))},
		{"a filter switched off and one left out", documents(indexSet(nativeDeclared(plainKeyword, plainTables)),
			indexSet(nativeHeld(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"KEYWORD","useFilterLowercase":true}`), plainTables)))},
		{"a declaration without an index set leaves a vector index alone", documents("", indexSet(nativeHeld(docsEmbRead, prefixedTables)))},
		{"a source of a host without index kinds leaves a vector index alone", withoutFeatures(statedNone(documents("", indexSet(nativeHeld(docsEmbRead, prefixedTables)))))},
		{"a source that could not describe native index sets leaves a vector index alone",
			unknownNative(statedNone(documents("", indexSet(nativeHeld(docsEmbRead, prefixedTables)))))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// statedNone is a case whose source states the native index set of every
// table it declares, for a host that negotiates source.index-kinds, and
// declares none for the table.
func statedNone(in alterCase) alterCase {
	in.nativeKnowledge, in.features = protocol.ModelKnowledge_COMPLETE, []string{provider.IndexKindsFeature}
	return in
}

// withoutFeatures is a case of a host that negotiates no target feature
// beside planning lifecycle.
func withoutFeatures(in alterCase) alterCase {
	in.features = nil
	return in
}

// unknownNative is a case whose source could not describe the native index
// sets of the tables it declares.
func unknownNative(in alterCase) alterCase {
	in.nativeKnowledge = protocol.ModelKnowledge_UNINSPECTED
	return in
}

// withCapabilities is a case on a target that has every vector and full-text
// capability but the ones set false.
func withCapabilities(in alterCase, set map[string]bool) alterCase {
	in.capabilities = everyIndexKind()
	maps.Copy(in.capabilities, set)
	return in
}

// Each vector or full-text index the planner refuses before a statement is
// sent, in the words of the host's embedded engine where it has them.
func TestNativeIndexPlan_FailurePath(t *testing.T) {
	bits := vectorJSON("docs_bits", []string{"emb"}, nil, kmeans("DISTANCE_MANHATTAN", "VECTOR_TYPE_BIT", 8, 1, 2), "")
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"a target without vector indexes", withCapabilities(documents(indexSet(nativeDeclared(docsEmb, prefixedTables)), indexSet()), map[string]bool{tablecreate.VectorIndexesCapability: false}),
			`index "docs_emb" declares vector settings, which requires target capability vector_indexes, unavailable on this ydb target .*EnableVectorIndex.*`},
		{"bit vectors on a target that does not build them", withCapabilities(documents(indexSet(nativeDeclared(bits, vectorTables)), indexSet()), map[string]bool{tablecreate.VectorBitTypeCapability: false}),
			`index "docs_bits" stores bit vectors, which requires target capability vector_bit_type, unavailable on this ydb target .*`},
		{"a target without full-text indexes", withCapabilities(documents(indexSet(nativeDeclared(standardBody, relevanceTables)), indexSet()), map[string]bool{tablecreate.FullTextIndexesCapability: false}),
			`index "ft" is a full-text index, which requires target capability full_text_indexes, unavailable on this ydb target .*EnableFulltextIndex.*`},
		{"a vector column that is not String", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"body"}, nil, cosine3(2), ""), vectorTables)), indexSet()),
			`index "v": its vector column "body" is Utf8, and a YDB vector index reads a String column .*`},
		{"a vector index without settings", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, `{}`, ""), vectorTables)), indexSet()),
			`index "v": a vector_kmeans_tree index declares none of its settings; declare its distance or similarity, vector_type, vector_dimension, levels and clusters`},
		{"a vector index without a vector type", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			`{"settings":{"metric":"DISTANCE_COSINE","vectorDimension":3},"levels":1,"clusters":2}`, ""), vectorTables)), indexSet()),
			`index "v": a vector index names its vector_type \(.vector_type should be set.\)`},
		{"a vector index without a dimension", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT"},"levels":1,"clusters":2}`, ""), vectorTables)), indexSet()),
			`index "v": a vector index's vector_dimension is between 1 and 16384 .*`},
		{"a vector dimension above the limit", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 16385, 1, 2), ""), vectorTables)), indexSet()),
			`index "v": a vector index's vector_dimension is between 1 and 16384 .*`},
		{"one cluster", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 3, 1, 1), ""), vectorTables)), indexSet()),
			`index "v": a vector index's clusters are between 2 and 2048 .*`},
		{"a prefix column YDB refuses as a key", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"score", "emb"}, nil, cosine3(2), ""), prefixedTables)), indexSet()),
			`index "v": column "score" is Double, which YDB refuses as an index key`},
		{"a key column covered", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"gen", "emb"}, []string{"gen"}, cosine3(2), ""), prefixedTables)), indexSet()),
			`index "v": column "gen" is both a key and a covered column, which YDB refuses`},
		{"an undeclared column covered", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, []string{"other"}, cosine3(2), ""), vectorTables)), indexSet()),
			`index "v" reads column "other", which the declaration does not state`},
		{"a vector index without levels", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"clusters":2}`, ""), vectorTables)), indexSet()),
			`index "v": a vector index's levels are between 1 and 16 \(.levels should be set.\).*`},
		{"a vector tree too large", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 3, 16, 2048), ""), vectorTables)), indexSet()),
			`index "v": a vector index's clusters to the power of its levels is at most 1073741824 .*`},
		{"a vector index without a metric", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			`{"settings":{"vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":2}`, ""), vectorTables)), indexSet()),
			`index "v": a vector index names one metric, distance or similarity .*`},
		{"a vector index with overlapping clusters", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":2,"overlapClusters":2}`, ""), vectorTables)), indexSet()),
			`index "v": this provider writes no overlap_clusters or overlap_ratio .*`},
		{"tuned implementation tables", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, cosine3(2),
			`,"levelTableSettings":{"partitioningSettings":{"minPartitionsCount":"4"}}`), vectorTables)), indexSet()),
			`index "v": it states the partitioning of a table that implements it, which this provider does not write; .*`},
		{"a vector index on an undeclared column", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"other"}, nil, cosine3(2), ""), vectorTables)), indexSet()),
			`index "v" reads column "other", which the declaration does not state`},
		{"a vector index that covers a key column", documents(indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, []string{"id"}, cosine3(2), ""), vectorTables)), indexSet()),
			`index "v": it covers key column "id", which every YDB index carries already`},
		{"a full-text index on two columns", documents(indexSet(nativeDeclared(`"name":"ft","indexColumns":["title","body"],"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[{"column":"body","analyzers":{"tokenizer":"STANDARD"}}]}}`, plainTables)), indexSet()),
			`index "ft": a YDB full-text index requires exactly one text column; this release does not support prefix columns`},
		{"a full-text index on a number", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "gen", `{"tokenizer":"STANDARD"}`), plainTables)), indexSet()),
			`index "ft": its text column "gen" is Int64, and a YDB full-text index reads a String or Utf8 column`},
		{"a full-text index without a tokenizer", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"useFilterLowercase":true}`), plainTables)), indexSet()),
			`index "ft": a YDB full-text index requires tokenizer`},
		{"a full-text index that analyzes another column", documents(indexSet(nativeDeclared(`"name":"ft","indexColumns":["body"],"globalFulltextPlainIndex":{"fulltextSettings":{"columns":[{"column":"title","analyzers":{"tokenizer":"STANDARD"}}]}}`, plainTables)), indexSet()),
			`index "ft": a YDB full-text index requires analyzer settings in WITH for its column "body"`},
		{"an n-gram filter without its minimum length", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"STANDARD","useFilterEdgeNgram":true,"filterNgramMaxLength":4}`), plainTables)), indexSet()),
			`index "ft": a YDB n-gram filter requires filter_ngram_min_length`},
		{"a negative length", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"STANDARD","useFilterLength":true,"filterLengthMin":-1}`), plainTables)), indexSet()),
			`index "ft": a full-text index's lengths are not negative`},
		{"an n-gram filter without its lengths", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"STANDARD","useFilterNgram":true,"filterNgramMinLength":3}`), plainTables)), indexSet()),
			`index "ft": a YDB n-gram filter requires filter_ngram_max_length`},
		{"a language without the snowball filter", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"STANDARD","language":"english"}`), plainTables)), indexSet()),
			`index "ft": a full-text index's language goes with use_filter_snowball .*`},
		{"the snowball filter with an n-gram filter", documents(indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body",
			`{"tokenizer":"STANDARD","useFilterSnowball":true,"useFilterEdgeNgram":true,"filterNgramMinLength":3,"filterNgramMaxLength":4}`), plainTables)), indexSet()),
			`index "ft": a full-text index takes no n-gram filter with use_filter_snowball .*`},
		// Ported from the host's TestVectorIndexService_ChangesAnIndexBuiltOtherwise.
		{"vector settings on an index the table holds as a global one", documents(indexSet(nativeDeclared(docsEmb, prefixedTables)),
			indexSet(nativeHeld(indexJSON("globalIndex", "docs_emb", []string{"gen"}, nil), plainTables))),
			`index "docs_emb": the table holds a global index and the declaration states a vector index under its name; .*`},
		{"a global index under the name of a vector one", documents(indexSet(nativeDeclared(indexJSON("globalIndex", "docs_emb", []string{"gen"}, nil), plainTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables))),
			`index "docs_emb": the table holds a vector index and the declaration states a global index under its name; .*`},
		{"a kept vector index reads a column the declaration drops", alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"gen", "emb"}, held: []string{"gen", "emb", "body"},
			types: map[string]string{"gen": "Int64", "emb": "String"}, capabilities: everyIndexKind(),
			desiredIndexes: indexSet(nativeDeclared(docsEmb, prefixedTables)), heldIndexes: indexSet(nativeHeld(docsEmbRead, prefixedTables))},
			`index "docs_emb" reads column "body", which the declaration does not state`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// A full-text index needs a table whose key is one Uint64 column: YDB 26.2
// answers `Fulltext index requires primary key column 'id' to be of type
// 'Uint64' but got Int64`.
func TestNativeIndexPlan_FullTextNeedsAUint64Key(t *testing.T) {
	c := testcheck.New(t)
	request := alterRequest(c, documents(indexSet(nativeDeclared(standardBody, relevanceTables)), indexSet()))
	request.Tables[0].Desired.Columns[0].Type = "Int64"
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `index "ft": a YDB full-text index requires exactly one Uint64 primary key column .*`)
}

// vectorIndexOf is the index set that declares docs_emb with settings.
func vectorIndexOf(settings string) string {
	return indexSet(nativeDeclared(vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, settings, ""), prefixedTables))
}

// The comparison reads a vector or full-text index as the planner does, so a
// difference it reports is one the planner acts on, and a read of what was
// declared reports none.
func TestNativeIndexComparison(t *testing.T) {
	for _, test := range []struct {
		name          string
		desired, held string
		differs       bool
	}{
		{"a vector index read back with its implementation settings", indexSet(nativeDeclared(docsEmb, prefixedTables)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), false},
		{"other clusters", indexSet(nativeDeclared(vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, cosine3(4), ""), prefixedTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"another name", indexSet(nativeDeclared(vectorJSON("docs_emb_cosine", []string{"gen", "emb"}, []string{"body"}, cosine3(2), ""), prefixedTables)),
			indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"a vector index the declaration leaves out", indexSet(), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"a filter switched off and one left out", indexSet(nativeDeclared(plainKeyword, plainTables)),
			indexSet(nativeHeld(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"KEYWORD","useFilterLowercase":true}`), plainTables)), false},
		{"another tokenizer", indexSet(nativeDeclared(keywordBody, relevanceTables)), indexSet(nativeHeld(standardBody, relevanceTables)), true},
		{"a plain index in place of a relevance one", indexSet(nativeDeclared(plainKeyword, plainTables)), indexSet(nativeHeld(keywordBody, relevanceTables)), true},
		// Each vector setting is part of the index. Ported from the host's
		// TestVectorIndexService_ChangesAnIndexBuiltOtherwise.
		{"another distance", vectorIndexOf(kmeans("DISTANCE_EUCLIDEAN", "VECTOR_TYPE_FLOAT", 3, 1, 2)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"a similarity of the same name", vectorIndexOf(kmeans("SIMILARITY_COSINE", "VECTOR_TYPE_FLOAT", 3, 1, 2)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"another element type", vectorIndexOf(kmeans("DISTANCE_COSINE", "VECTOR_TYPE_UINT8", 3, 1, 2)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"another dimension", vectorIndexOf(kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 4, 1, 2)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"other levels", vectorIndexOf(kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 3, 2, 2)), indexSet(nativeHeld(docsEmbRead, prefixedTables)), true},
		{"an index 25.1 reads without levels and clusters", indexSet(nativeDeclared(docsEmb, prefixedTables)), indexSet(nativeHeld(vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"},
			`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3}}`, ""), prefixedTables)), true},
		{"vector settings on an index read as a global one", indexSet(nativeDeclared(docsEmb, prefixedTables)),
			indexSet(nativeHeld(indexJSON("globalIndex", "docs_emb", []string{"gen", "emb"}, []string{"body"}), plainTables)), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			differs, err := tablecreate.FacetDiffers(facet(c, schema.IndexProperties, "", test.desired).Value.Value,
				observedFacet(c, schema.IndexProperties, "", test.held).Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(differs, qt.Equals, test.differs)
		})
	}
}

// A change of a vector index's settings needs the index the table holds, so
// an altered table read without its index set, as a rollback that could not
// capture the table sends it, plans no step blind: the change is refused.
// Ported from the host's TestVectorIndexService_RefusesAChangeWithoutACapture.
func TestNativeIndexPlan_RefusesAChangeWithoutACapture(t *testing.T) {
	c := testcheck.New(t)
	request := alterRequest(c, documents(indexSet(nativeDeclared(vectorJSON("docs_emb", []string{"gen", "emb"}, []string{"body"}, cosine3(4), ""), prefixedTables)), ""))

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, "The table's current indexes were not captured")
}
