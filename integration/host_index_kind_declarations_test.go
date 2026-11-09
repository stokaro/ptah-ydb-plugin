//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The index declarations of this file are the host's YDB vector, full-text and
// local index tests at 74de0b65b (integration/dbschema/ydb/vector_indexes_live_test.go,
// vector_indexes_compat_e2e_test.go, fulltext_live_test.go and
// column_store_live_test.go), written as Go, YAML and HCL sources: a host that
// negotiates source.index-kinds sends each declaration of a vector, full-text
// or local type whole, and the provider puts it in the table's native index set
// or its column storage.

// docsTable declares docs in directory, keyed by id, with a generation, a
// vector, a body and the further fields given, and the index directives given.
func docsTable(directory, fields, indexes string) string {
	return "package entities\n\n//ptah:schema:table name=\"docs\" schema=\"" + directory + "\"\ntype Doc struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"gen\" type=\"Int64\"\n\tGen *int64\n" +
		"\t//ptah:schema:field name=\"emb\" type=\"String\"\n\tEmb []byte\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody *string\n" + fields + indexes + "}\n"
}

// docsEmb is the vector index over the generation and the vector that covers
// the body, with clusters clusters, named name.
func docsEmb(name, clusters string) string {
	return "\t//ptah:schema:index name=\"" + name + "\" fields=\"gen,emb\" include=\"body\" type=\"vector_kmeans_tree\" distance=\"cosine\" vector_type=\"float\"" +
		" vector_dimension=\"3\" levels=\"1\" clusters=\"" + clusters + "\"\n\t_ struct{}\n"
}

const (
	// titleField is a second vector column.
	titleField = "\t//ptah:schema:field name=\"title_emb\" type=\"String\"\n\tTitleEmb []byte\n"
	// titleIP is a vector index over it that names its metric by pgvector's
	// operator class, as the host's embedded engine read it.
	titleIP = "\t//ptah:schema:index name=\"docs_title_ip\" fields=\"title_emb\" type=\"vector_kmeans_tree\" ops=\"vector_ip_ops\" vector_type=\"int8\"" +
		" vector_dimension=\"3\" levels=\"2\" clusters=\"2\"\n\t_ struct{}\n"
)

// docsFixture is a locked fixture directory whose docs table the test drops.
func docsFixture(c *qt.C) (opened *connection.Connection, directory, absolute string) {
	c.Helper()
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "docs")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	return opened, directory, absolute
}

// nearestDoc asks index of the table at absolute for the key of the row of
// generation 1 nearest [1, 0, 0], through the index.
func nearestDoc(c *qt.C, opened *connection.Connection, absolute, index string) int64 {
	c.Helper()
	var id int64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT id FROM `"+absolute+"` VIEW `"+index+"` WHERE gen = 1l "+
		"ORDER BY Knn::CosineDistance(emb, Knn::ToBinaryStringFloat([1.0f, 0.0f, 0.0f])) LIMIT 1").Scan(&id), qt.IsNil)
	return id
}

// statements are the ALTER TABLE statements of a plan in the order they run,
// as the host's preview of the plan prints them, without the directory.
func statements(output, directory string) []string {
	var listed []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "ALTER TABLE `") {
			listed = append(listed, strings.TrimSuffix(strings.ReplaceAll(line, directory+"/", ""), ";"))
		}
	}
	return listed
}

// kindRun is one run of a declaration and what its plan runs.
type kindRun struct {
	name, source string
	want         []string
}

// applyKindRuns applies each run's source in directory and asserts the ALTER
// TABLE statements its plan runs, none for a run that finds the table synced.
func applyKindRuns(c *qt.C, directory string, runs []kindRun) {
	c.Helper()
	for _, run := range runs {
		output, err := hostApply(c, directory, run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(statements(output, directory), qt.DeepEquals, run.want, qt.Commentf("%s: %s", run.name, output))
	}
}

// The host's TestYDBVectorIndexes_RoundTrip, through a Go source: a vector
// index added to a table holding rows answers a search, a changed setting and
// a rename each drop an index and add one, a vector column and its index
// come in one plan, the column first, and go in one, the index first, the last
// index goes when the source states none, and every next run is synced, on
// both release lines.
func TestHostVectorIndexesFromGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := docsFixture(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, gen Int64, emb String, body Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, gen, emb, body) VALUES "+
		"(1l, 1l, "+floatVector(0, 1, 0)+", 'a'u), (2l, 1l, "+floatVector(0, 0, 1)+", 'b'u), "+
		"(3l, 1l, "+floatVector(0.9, 0.1, 0)+", 'c'u), (4l, 2l, "+floatVector(1, 0, 0)+", 'd'u)"), qt.IsNil)
	add := func(name, clusters string) string {
		return "ALTER TABLE `docs` ADD INDEX `" + name + "` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) " +
			"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=" + clusters + ")"
	}
	applyKindRuns(c, directory, []kindRun{
		{"the index added", docsTable(directory, "", docsEmb("docs_emb", "2")), []string{add("docs_emb", "2")}},
		{"a second run", docsTable(directory, "", docsEmb("docs_emb", "2")), nil},
	})
	c.Assert(nearestDoc(c, opened, absolute, "docs_emb"), qt.Equals, int64(3))
	applyKindRuns(c, directory, []kindRun{
		{"a changed setting", docsTable(directory, "", docsEmb("docs_emb", "4")), []string{"ALTER TABLE `docs` DROP INDEX `docs_emb`", add("docs_emb", "4")}},
		{"a run after the change", docsTable(directory, "", docsEmb("docs_emb", "4")), nil},
		// The host's embedded engine dropped the old index first; under two
		// names the steps are independent, and the plan adds the new one first.
		{"the index renamed", docsTable(directory, "", docsEmb("docs_emb_cosine", "4")), []string{add("docs_emb_cosine", "4"), "ALTER TABLE `docs` DROP INDEX `docs_emb`"}},
		{"a run after the rename", docsTable(directory, "", docsEmb("docs_emb_cosine", "4")), nil},
	})
	c.Assert(nearestDoc(c, opened, absolute, "docs_emb_cosine"), qt.Equals, int64(3))
	applyKindRuns(c, directory, []kindRun{
		{"a vector column and its index", docsTable(directory, titleField, docsEmb("docs_emb_cosine", "4")+titleIP), []string{"ALTER TABLE `docs` ADD COLUMN `title_emb` String",
			"ALTER TABLE `docs` ADD INDEX `docs_title_ip` GLOBAL USING vector_kmeans_tree ON (`title_emb`) WITH (similarity=inner_product, vector_type=int8, " +
				"vector_dimension=3, levels=2, clusters=2)"}},
		{"a run after the addition", docsTable(directory, titleField, docsEmb("docs_emb_cosine", "4")+titleIP), nil},
		{"both gone again", docsTable(directory, "", docsEmb("docs_emb_cosine", "4")), []string{"ALTER TABLE `docs` DROP INDEX `docs_title_ip`",
			"ALTER TABLE `docs` DROP COLUMN `title_emb`"}},
		{"a run after the removal", docsTable(directory, "", docsEmb("docs_emb_cosine", "4")), nil},
		// A source that states no vector index for a table states that the
		// table has none.
		{"the last index left out", docsTable(directory, "", ""), []string{"ALTER TABLE `docs` DROP INDEX `docs_emb_cosine`"}},
		{"a run without indexes", docsTable(directory, "", ""), nil},
	})
}

// The host's TestYDBVectorIndexes_CreatedWithTheTable, through Go, YAML and
// HCL sources: a new table's vector indexes are created with it, inside its
// CREATE TABLE as the host's embedded engine wrote them, so no statement adds
// one to the table that exists, and the next run is synced, on both release
// lines.
func TestHostVectorIndexesCreatedWithTheTable_HappyPath(t *testing.T) {
	const yamlSource = `tables:
  docs:
    schema: {directory}
    columns:
      id: {type: Int64, primary: true, not_null: true}
      gen: {type: Int64}
      emb: {type: String}
      body: {type: Utf8}
    indexes:
      docs_emb: {fields: [gen, emb], include: [body], type: vector_kmeans_tree, distance: cosine, vector_type: float, vector_dimension: 3, levels: 1, clusters: 2}
`
	const hclSource = `schema "{directory}" {}
table "docs" {
  schema = schema.{directory}
  column "id" {
    type = Int64
  }
  column "gen" {
    type = Int64
    null = true
  }
  column "emb" {
    type = String
    null = true
  }
  column "body" {
    type = Utf8
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  index "docs_emb" {
    type             = "vector_kmeans_tree"
    columns          = [column.gen, column.emb]
    include          = [column.body]
    distance         = "cosine"
    vector_type      = "float"
    vector_dimension = 3
    levels           = 1
    clusters         = 2
  }
}
`
	created := "    INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) " +
		"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2)"
	for _, test := range []struct{ name, format, source string }{
		{"Go", "go", docsTable("{directory}", "", docsEmb("docs_emb", "2"))},
		{"YAML", "yaml", yamlSource},
		{"HCL", "hcl", hclSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, directory, _ := docsFixture(c)
			source := strings.ReplaceAll(test.source, "{directory}", directory)
			output, err := hostApplySource(c, directory, test.format, source)
			c.Assert(err, qt.IsNil, qt.Commentf("the creation: %s", output))
			c.Assert(statements(output, directory), qt.HasLen, 0, qt.Commentf("the creation: %s", output))
			c.Assert(output, qt.Contains, created, qt.Commentf("the creation: %s", output))
			again, err := hostApplySource(c, directory, test.format, source)
			c.Assert(err, qt.IsNil, qt.Commentf("a second run: %s", again))
			c.Assert(statements(again, directory), qt.HasLen, 0, qt.Commentf("a second run: %s", again))
			c.Assert(pluginUnits(again), qt.HasLen, 0, qt.Commentf("a second run: %s", again))
		})
	}
}

// The host's TestYDBVectorIndexes_ApplyBackFromInspectedHCL, the HCL contents
// included: `ptah-compat schema inspect` writes a vector index's block as the
// host's embedded engine wrote it, with its type, the column it covers and
// its settings, and the document applied back finds the table synced, on
// both release lines.
func TestHostCompatInspectedHCLCarriesTheVectorIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	_, directory, _ := docsFixture(c)
	output, err := hostApply(c, directory, docsTable(directory, titleField, docsEmb("docs_emb", "2")+titleIP))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	document, _, reapplied := compatInspect(c, directory)

	c.Assert(document, qt.Contains, "    type = \"GLOBAL USING vector_kmeans_tree\"\n    include = [column.body]\n"+
		"    distance = \"cosine\"\n    vector_type = \"float\"\n    vector_dimension = 3\n    levels = 1\n    clusters = 2\n")
	c.Assert(document, qt.Contains, "    type = \"GLOBAL USING vector_kmeans_tree\"\n    similarity = \"inner_product\"\n    vector_type = \"int8\"\n"+
		"    vector_dimension = 3\n    levels = 2\n    clusters = 2\n    columns = [column.title_emb]\n")
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
}

// docsFullText declares docs in directory keyed by a Uint64 id, as a
// full-text index needs, with a full-text index of kind on body and its
// analyzers as attributes.
func docsFullText(directory, kind, analyzers string) string {
	return "package entities\n\n//ptah:schema:table name=\"docs\" schema=\"" + directory + "\"\ntype Doc struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\t//ptah:schema:index name=\"ft\" fields=\"body\" type=\"" + kind + "\"" + analyzers + "\n\tBody *string\n}\n"
}

// The host's TestYDBFullText_RoundTrip, through a Go source on a line with
// full-text indexes (26.2): the index is created with its analyzers and with
// its table, inside the CREATE TABLE as the host's embedded engine wrote it,
// so no statement adds it; another tokenizer and another kind each drop it and
// add it again, and every next run is synced. `ptah-compat schema inspect` writes the analyzers in the index
// block and the document applied back is synced, where the host's embedded
// engine wrote none and refused its own document (`a YDB full-text index
// requires analyzer settings in WITH`).
func TestHostFullTextIndexFromGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireSessionCapability(c, session, tablecreate.FullTextIndexesCapability)
	_, directory, _ := docsFixture(c)
	add := func(kind, analyzers string) string {
		return "ALTER TABLE `docs` ADD INDEX `ft` GLOBAL USING " + kind + " ON (`body`) WITH (" + analyzers + ")"
	}
	relevance := func(tokenizer string) string {
		return docsFullText(directory, "fulltext_relevance", ` tokenizer="`+tokenizer+`" use_filter_lowercase="true"`)
	}
	plain := docsFullText(directory, "fulltext_plain", ` tokenizer="keyword" use_filter_lowercase="true" use_filter_stopwords="false"`)
	applyKindRuns(c, directory, []kindRun{
		{"the table created with the index", relevance("standard"), nil},
		{"a second run", relevance("standard"), nil},
		{"another tokenizer", relevance("keyword"), []string{"ALTER TABLE `docs` DROP INDEX `ft`", add("fulltext_relevance", "tokenizer=keyword, use_filter_lowercase=true")}},
		{"a run after the tokenizer", relevance("keyword"), nil},
		{"another kind", plain, []string{"ALTER TABLE `docs` DROP INDEX `ft`", add("fulltext_plain", "tokenizer=keyword, use_filter_lowercase=true, use_filter_stopwords=false")}},
		{"a run after the kind", plain, nil},
	})
	hostCompat(c)

	document, _, reapplied := compatInspect(c, directory)

	c.Assert(document, qt.Contains, "    type = \"GLOBAL USING fulltext_plain\"\n    tokenizer = \"keyword\"\n    use_filter_lowercase = \"true\"\n"+
		"    use_filter_stopwords = \"false\"\n    columns = [column.body]\n")
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
}

// The host's TestYDBFullText_OlderLineRefuses, through a Go source: on a line
// without full-text indexes (25.1) the plan refuses the index before a
// statement is sent, in the host's embedded engine's words.
func TestHostFullTextIndexOnAnOlderLine_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireNoSessionCapability(c, session, tablecreate.FullTextIndexesCapability)
	_, directory, _ := docsFixture(c)

	output, err := hostApply(c, directory, docsFullText(directory, "fulltext_relevance", ` tokenizer="standard"`))

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*index "ft" is a full-text index, which requires target capability full_text_indexes, unavailable on this ydb target.*`)
}

// eventsTable declares events in directory, a column table hashed by id in one
// shard, with a body and the local index directives given.
func eventsTable(directory, indexes string) string {
	return "package entities\n\n//ptah:schema:table name=\"events\" schema=\"" + directory + "\" store=\"COLUMN\" partition_by_hash=\"id\" column_shards=\"1\"\n" +
		"type Event struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n" + indexes + "\tBody *string\n}\n"
}

// bloom is a bloom_filter index of body named name, with probability.
func bloom(name, probability string) string {
	return "\t//ptah:schema:index name=\"" + name + "\" fields=\"body\" type=\"bloom_filter\" false_positive_probability=\"" + probability + "\"\n"
}

// ngram is a bloom_ngram_filter index of body that states no setting.
const ngram = "\t//ptah:schema:index name=\"ng\" fields=\"body\" type=\"bloom_ngram_filter\"\n"

// The host's TestYDBColumnStore_RoundTrip and TestYDBColumnStore_IndexChanges,
// the local indexes, through a Go source on a line with them (26.2): a new
// column table is created with its local indexes, a renamed index is renamed
// in place, a changed probability drops the index and adds it again, an index
// the source leaves out is dropped, the last one too, and every next run is
// synced, as the host's embedded engine plans each.
func TestHostLocalIndexesFromGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireSessionCapability(c, session, tablecreate.LocalBloomIndexesCapability)
	requireSessionCapability(c, session, tablecreate.LocalNgramIndexesCapability)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	created, err := hostApply(c, directory, eventsTable(directory, bloom("bf", "0.01")+ngram))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Matches, "(?s).*INDEX `bf` LOCAL USING bloom_filter ON \\(`body`\\) WITH \\(false_positive_probability = 0.01\\),\n"+
		"    INDEX `ng` LOCAL USING bloom_ngram_filter ON \\(`body`\\) WITH \\(case_sensitive = true, false_positive_probability = 0.1, ngram_size = 3\\).*")
	applyKindRuns(c, directory, []kindRun{
		{"a second run", eventsTable(directory, bloom("bf", "0.01")+ngram), nil},
		{"an index renamed", eventsTable(directory, bloom("renamed_bloom", "0.01")+ngram), []string{"ALTER TABLE `events` RENAME INDEX `bf` TO `renamed_bloom`"}},
		{"a run after the rename", eventsTable(directory, bloom("renamed_bloom", "0.01")+ngram), nil},
		{"another probability", eventsTable(directory, bloom("renamed_bloom", "0.02")+ngram), []string{"ALTER TABLE `events` DROP INDEX `renamed_bloom`",
			"ALTER TABLE `events` ADD INDEX `renamed_bloom` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.02)"}},
		{"a run after the probability", eventsTable(directory, bloom("renamed_bloom", "0.02")+ngram), nil},
		{"an index left out", eventsTable(directory, bloom("renamed_bloom", "0.02")), []string{"ALTER TABLE `events` DROP INDEX `ng`"}},
		{"a run after the removal", eventsTable(directory, bloom("renamed_bloom", "0.02")), nil},
		// The source states every local index of the column table, none here.
		{"every index left out", eventsTable(directory, ""), []string{"ALTER TABLE `events` DROP INDEX `renamed_bloom`"}},
		{"a run without indexes", eventsTable(directory, ""), nil},
	})
}

// The host's Go export of a column table, as its embedded engine wrote it at
// 74de0b65b, on a line with local indexes (26.2): `ptah introspect` writes the
// table directive with the hash key and the shard count, and each local index
// with its settings. The data channels YDB gives the table, and the storage
// and the encodings it gives each index, are what a declaration without them
// creates, so the export loses nothing, and the file applied back finds the
// table synced, without local indexes and with them.
func TestHostIntrospectsAColumnTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireSessionCapability(c, session, tablecreate.LocalBloomIndexesCapability)
	requireSessionCapability(c, session, tablecreate.LocalNgramIndexesCapability)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	// The export writes store="column", as the host's embedded engine did;
	// the source the test applies spells it COLUMN, which reads the same.
	table := `//ptah:schema:table name="events" schema="` + directory + `" store="column" partition_by_hash="id" column_shards="1"`
	for _, step := range []struct {
		name, indexes string
		want          []string
	}{
		{"without local indexes", "", []string{table}},
		{"with local indexes", bloom("bf", "0.01") + ngram, []string{table,
			`//ptah:schema:index name="bf" fields="body" type="bloom_filter" false_positive_probability="0.01"`,
			`//ptah:schema:index name="ng" fields="body" type="bloom_ngram_filter" false_positive_probability="0.1" ngram_size="3" case_sensitive="true"`}},
	} {
		created, err := hostApply(c, directory, eventsTable(directory, step.indexes))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, created))
		written := introspectDirectory(c, directory)
		for _, line := range step.want {
			c.Assert(written, qt.Contains, line+"\n", qt.Commentf("%s: %s", step.name, written))
		}
		output, err := hostApply(c, directory, written)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, output))
		c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s", step.name))
	}
}

// The host's column store tests on a line without local indexes (25.1): the
// plan refuses a local index before a statement is sent, naming the
// capability.
func TestHostLocalIndexOnAnOlderLine_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	requireNoSessionCapability(c, session, tablecreate.LocalBloomIndexesCapability)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	_, directory, _ := lockFixture(c)

	output, err := hostApply(c, directory, eventsTable(directory, bloom("bf", "0.01")))

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*local index "bf" is a bloom_filter index, which requires target capability local_bloom_indexes, unavailable on this ydb target.*`)
}
