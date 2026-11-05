//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
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

// nativeIndex is one declaration of a native index set: the specification and
// a requirement on each table that implements it, which the planner does not
// read.
func nativeIndex(spec string, tables []string) string {
	requirements := make([]string, len(tables))
	for i, table := range tables {
		requirements[i] = `{"name":"` + table + `","settings":{"primaryKey":["id"]},"columns":[{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}`
	}
	return `{"specification":{` + spec + `},"implementation":[` + strings.Join(requirements, ",") + `]}`
}

func nativeIndexSet(entries ...string) string {
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

// vectorIndex states a vector index of float vectors of three elements
// compared by cosine distance, on one level of clusters clusters.
func vectorIndex(name, keys, cover string, clusters int) string {
	return vectorIndexWith(name, keys, cover, fmt.Sprintf(`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":%d}`, clusters))
}

func vectorIndexWith(name, keys, cover, settings string) string {
	text := `"name":"` + name + `","indexColumns":` + keys + `,"globalVectorKmeansTreeIndex":{"vectorSettings":` + settings + `}`
	if cover != "" {
		text += `,"dataColumns":` + cover
	}
	return text
}

// fullTextIndex states a full-text index of kind on column with analyzers.
func fullTextIndex(kind, name, column, analyzers string) string {
	return `"name":"` + name + `","indexColumns":["` + column + `"],"` + kind + `":{"fulltextSettings":{"columns":[{"column":"` + column + `","analyzers":` + analyzers + `}]}}`
}

// newDocumentsFixture is a facet fixture whose records also hold a
// generation, a vector and a body, the columns of a table of documents. Its
// provider renders the TEXT form too.
func newDocumentsFixture(c *qt.C) *facetFixture {
	c.Helper()
	fixture := newFacetFixtureListing(c, ", gen Int64, emb String, body Utf8", "", provider.TextUnitsFeature)
	fixture.columns = documentColumns()
	return fixture
}

func documentColumns() []*protocol.DeclaredColumn {
	return []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true},
		{Name: "payload", Type: "Utf8", Nullable: true}, {Name: "gen", Type: "Int64", Nullable: true}, {Name: "emb", Type: "String", Nullable: true},
		{Name: "body", Type: "Utf8", Nullable: true}}
}

// floatVector writes a vector of three float elements as the String a vector
// column stores.
func floatVector(x, y, z float64) string {
	return fmt.Sprintf(`Untag(Knn::ToBinaryStringFloat([CAST(%g AS Float), CAST(%g AS Float), CAST(%g AS Float)]), "FloatVector")`, x, y, z)
}

// recordsPath is the absolute path of the fixture's table, quoted.
func (f *facetFixture) recordsPath() string {
	return "`" + path.Join(f.database, f.directory, "records") + "`"
}

// exec runs one statement against the fixture's database.
func (f *facetFixture) exec(c *qt.C, statement string) error {
	c.Helper()
	opened := f.connection(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	return execFixtureSchema(ctx, opened.DB, statement)
}

// connection opens the fixture's database, closed when the test ends.
func (f *facetFixture) connection(c *qt.C) *connection.Connection {
	c.Helper()
	address, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	opened, err := connection.Open(c.Context(), address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	return opened
}

// nearest asks index for the key of the row of generation 1 nearest
// [1, 0, 0]. A search through a prefixed index names the prefix: without it
// YDB answers `Given predicate is not suitable for used index`.
func (f *facetFixture) nearest(c *qt.C, index string) uint64 {
	c.Helper()
	opened := f.connection(c)
	var id uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT id FROM "+f.recordsPath()+" VIEW `"+index+"` WHERE gen = 1l "+
		"ORDER BY Knn::CosineDistance(emb, Knn::ToBinaryStringFloat([1.0f, 0.0f, 0.0f])) LIMIT 1").Scan(&id), qt.IsNil)
	return id
}

// planned is a plan's steps in the order they run, by name, and the TEXT
// form of each, the statement a migration file holds.
func (f *facetFixture) planned(c *qt.C, plan *protocol.PlannedFeatures) (names, statements []string) {
	c.Helper()
	steps := scheduled(c, plan)
	for _, step := range steps {
		names = append(names, step.Id.Name)
	}
	rendering := renderSteps(steps, f.session.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := f.client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	for _, unit := range rendered.Units {
		statements = append(statements, strings.ReplaceAll(unit.GetSql(), f.directory+"/", "<dir>/"))
	}
	return names, statements
}

// indexPlan plans the native index set declared against the live table,
// with the host's steps the fixture carries, and requires no refusal.
func (f *facetFixture) indexPlan(c *qt.C, declared string) *protocol.PlannedFeatures {
	c.Helper()
	found := f.compareIndexes(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	plan := f.plan(c, found.Changes, desiredFacet(c, f.client, f.owner, schema.TableProperties, noTTLDeclaration),
		desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	return plan
}

// run executes a plan and requires it applied.
func (f *facetFixture) run(c *qt.C, id string, plan *protocol.PlannedFeatures) {
	c.Helper()
	execution := f.execute(c, id, plan)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
}

// requireSessionCapability skips a test on a target whose session does not
// report key: the suite runs on every release line, and a line without the
// capability runs the refusal tests instead.
func requireSessionCapability(c *qt.C, session *protocol.Session, key string) {
	c.Helper()
	if !session.Capabilities[key] {
		c.Skipf("the target's %s is false; this test needs true", key)
	}
}

// requireNoSessionCapability skips a refusal test on a target whose session
// reports key, which runs the tests of the capability instead.
func requireNoSessionCapability(c *qt.C, session *protocol.Session, key string) {
	c.Helper()
	if session.Capabilities[key] {
		c.Skipf("the target's %s is true; this test needs false", key)
	}
}

const addDocsEmb = "ALTER TABLE `<dir>/records` ADD INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) " +
	"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=%d);"

// TestVectorIndexRoundTripSubprocess is the host's
// TestYDBVectorIndexes_RoundTrip. It adds a vector index over a table that
// holds rows, reads it back, plans nothing after, and searches through it to
// show the index the server built is one. Then it changes a setting, which
// YDB makes only by a drop and an add, renames the index, which builds it
// again under its new name, adds a vector column with an index in one plan
// and drops both again, each step ending with nothing left to plan.
func TestVectorIndexRoundTripSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newDocumentsFixture(c)
	c.Assert(fixture.session.Capabilities[tablecreate.VectorIndexesCapability], qt.IsTrue,
		qt.Commentf("the test servers start 25.1 with EnableVectorIndex, and the session reads it"))
	c.Assert(fixture.exec(c, "UPSERT INTO "+fixture.recordsPath()+" (id, gen, emb, body) VALUES "+
		"(1ul, 1l, "+floatVector(0, 1, 0)+", 'a'u), (2ul, 1l, "+floatVector(0, 0, 1)+", 'b'u), "+
		"(3ul, 1l, "+floatVector(0.9, 0.1, 0)+", 'c'u), (4ul, 2l, "+floatVector(1, 0, 0)+", 'd'u)"), qt.IsNil)

	declared := nativeIndexSet(nativeIndex(vectorIndex("docs_emb", `["gen","emb"]`, `["body"]`, 2), prefixedTables))
	plan := fixture.indexPlan(c, declared)
	names, statements := fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-add-index/000000"})
	c.Assert(statements, qt.DeepEquals, []string{fmt.Sprintf(addDocsEmb, 2)})
	fixture.run(c, "vector-add", plan)
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	held := fixture.heldIndexes(c)["docs_emb"]
	c.Assert(held.Status, qt.Equals, native.TableIndexDescription_STATUS_READY)
	c.Assert(held.IndexColumns, qt.DeepEquals, []string{"gen", "emb"})
	c.Assert(held.DataColumns, qt.DeepEquals, []string{"body"})
	settings := held.GetGlobalVectorKmeansTreeIndex().GetVectorSettings()
	c.Assert(settings.GetSettings().GetMetric(), qt.Equals, native.VectorIndexSettings_DISTANCE_COSINE)
	c.Assert(settings.GetSettings().GetVectorType(), qt.Equals, native.VectorIndexSettings_VECTOR_TYPE_FLOAT)
	c.Assert(settings.GetSettings().GetVectorDimension(), qt.Equals, uint32(3))
	c.Assert([]uint32{settings.GetLevels(), settings.GetClusters()}, qt.DeepEquals, []uint32{1, 2})
	c.Assert(fixture.nearest(c, "docs_emb"), qt.Equals, uint64(3))

	// No setting changes in place: the index is dropped and added again.
	declared = nativeIndexSet(nativeIndex(vectorIndex("docs_emb", `["gen","emb"]`, `["body"]`, 4), prefixedTables))
	plan = fixture.indexPlan(c, declared)
	names, statements = fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-add-index/000000"})
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `<dir>/records` DROP INDEX `docs_emb`;", fmt.Sprintf(addDocsEmb, 4)})
	fixture.run(c, "vector-retune", plan)
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	c.Assert(fixture.heldIndexes(c)["docs_emb"].GetGlobalVectorKmeansTreeIndex().GetVectorSettings().GetClusters(), qt.Equals, uint32(4))

	// A renamed vector index is built again under its new name.
	declared = nativeIndexSet(nativeIndex(vectorIndex("docs_emb_cosine", `["gen","emb"]`, `["body"]`, 4), prefixedTables))
	plan = fixture.indexPlan(c, declared)
	names, statements = fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-add-index/000000"})
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `<dir>/records` DROP INDEX `docs_emb`;",
		strings.Replace(fmt.Sprintf(addDocsEmb, 4), "`docs_emb`", "`docs_emb_cosine`", 1)})
	fixture.run(c, "vector-rename", plan)
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	c.Assert(fixture.nearest(c, "docs_emb_cosine"), qt.Equals, uint64(3))

	// A vector column and its index in one plan: the column first.
	titleIP := vectorIndexWith("docs_title_ip", `["title_emb"]`, "",
		`{"settings":{"metric":"SIMILARITY_INNER_PRODUCT","vectorType":"VECTOR_TYPE_INT8","vectorDimension":3},"levels":2,"clusters":2}`)
	withTitle := nativeIndexSet(nativeIndex(vectorIndex("docs_emb_cosine", `["gen","emb"]`, `["body"]`, 4), prefixedTables), nativeIndex(titleIP, vectorTables))
	fixture.columns = append(documentColumns(), &protocol.DeclaredColumn{Name: "title_emb", Type: "String", Nullable: true})
	addition := fixture.addColumnStep("add-title-emb", "title_emb")
	addition.AddedColumn.Type = "String"
	fixture.steps = []*protocol.CommonPlanningStep{addition}
	plan = fixture.indexPlan(c, withTitle)
	names, statements = fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-add-column/000000", "row-table-add-index/000000"})
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `<dir>/records` ADD COLUMN `title_emb` String;",
		"ALTER TABLE `<dir>/records` ADD INDEX `docs_title_ip` GLOBAL USING vector_kmeans_tree ON (`title_emb`) " +
			"WITH (similarity=inner_product, vector_type=int8, vector_dimension=3, levels=2, clusters=2);"})
	fixture.run(c, "vector-column-add", plan)
	fixture.steps = nil
	c.Assert(fixture.compareIndexes(c, withTitle).Changes, qt.HasLen, 0)

	// And both gone again: the index first.
	fixture.columns = documentColumns()
	fixture.steps = []*protocol.CommonPlanningStep{fixture.dropColumnStep("drop-title-emb", "title_emb")}
	plan = fixture.indexPlan(c, declared)
	names, statements = fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-drop-column/000000"})
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `<dir>/records` DROP INDEX `docs_title_ip`;",
		"ALTER TABLE `<dir>/records` DROP COLUMN `title_emb`;"})
	fixture.run(c, "vector-column-drop", plan)
	fixture.steps = nil
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	c.Assert(fixture.heldColumns(c), qt.DeepEquals, []string{"id", "ts", "payload", "gen", "emb", "body"})
}

// TestVectorIndexCreatedWithTheTableSubprocess is the host's
// TestYDBVectorIndexes_CreatedWithTheTable. A new table's vector indexes are
// built right after its creation, one step each, where the host's embedded
// engine writes them inside the CREATE TABLE: the native creation call of the
// pinned bindings carries no vector index. The table reads back with every
// setting declared, and a comparison afterward finds nothing to change.
func TestVectorIndexCreatedWithTheTableSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	absolute := path.Join(database, directory, "records")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	fixture := &facetFixture{client: client, session: session, directory: directory, database: database, columns: documentColumns(),
		owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}}
	titleIP := vectorIndexWith("docs_title_ip", `["emb"]`, "",
		`{"settings":{"metric":"SIMILARITY_INNER_PRODUCT","vectorType":"VECTOR_TYPE_INT8","vectorDimension":3},"levels":2,"clusters":2}`)
	declared := nativeIndexSet(nativeIndex(vectorIndex("docs_emb", `["gen","emb"]`, `["body"]`, 2), prefixedTables), nativeIndex(titleIP, vectorTables))
	table := &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"}, Columns: documentColumns(),
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
				Records: []*protocol.ComparisonRecord{desiredFacet(c, client, fixture.owner, schema.IndexProperties, declared)}}}}
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: negotiated(client), PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
		Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(fixture.owner), Action: protocol.PlanningTable_CREATE, Desired: table}}}
	prepareCapturedCreation(c, client, planning)
	plan, err := client.PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	names, statements := fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-create/000000", "row-table-create-index/000000", "row-table-create-index/000001"})
	c.Assert(statements[1:], qt.DeepEquals, []string{fmt.Sprintf(addDocsEmb, 2),
		"ALTER TABLE `<dir>/records` ADD INDEX `docs_title_ip` GLOBAL USING vector_kmeans_tree ON (`emb`) " +
			"WITH (similarity=inner_product, vector_type=int8, vector_dimension=3, levels=2, clusters=2);"})
	fixture.run(c, "vector-create", plan)
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	settings := fixture.heldIndexes(c)["docs_title_ip"].GetGlobalVectorKmeansTreeIndex().GetVectorSettings()
	c.Assert(settings.GetSettings().GetMetric(), qt.Equals, native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT)
	c.Assert(settings.GetSettings().GetVectorType(), qt.Equals, native.VectorIndexSettings_VECTOR_TYPE_INT8)
	c.Assert([]uint32{settings.GetSettings().GetVectorDimension(), settings.GetLevels(), settings.GetClusters()}, qt.DeepEquals, []uint32{3, 2, 2})
}

// TestVectorIndexFollowsTheLineSubprocess is the host's
// TestYDBVectorIndexes_FollowTheirKeys: the two keys a line answers
// differently to. A row written after the build is found through the index
// exactly where vector_index_maintained_on_write holds, and a plan for an
// index over bit vectors is refused by name where vector_bit_type does not
// hold, which is where the server does not build one over rows.
func TestVectorIndexFollowsTheLineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newDocumentsFixture(c)
	caps := fixture.session.Capabilities
	// The index is built over rows, the way a vector index is built over a
	// table in use, and a row is written after it.
	c.Assert(fixture.exec(c, "UPSERT INTO "+fixture.recordsPath()+" (id, gen, emb) VALUES "+
		"(1ul, 1l, "+floatVector(0, 1, 0)+"), (2ul, 1l, "+floatVector(0, 0, 1)+"), (3ul, 1l, "+floatVector(-1, 0, 0)+")"), qt.IsNil)
	declared := nativeIndexSet(nativeIndex(vectorIndex("docs_emb", `["gen","emb"]`, `["body"]`, 2), prefixedTables))
	fixture.run(c, "vector-add", fixture.indexPlan(c, declared))
	c.Assert(fixture.exec(c, "UPSERT INTO "+fixture.recordsPath()+" (id, gen, emb) VALUES (5ul, 1l, "+floatVector(1, 0, 0)+")"), qt.IsNil)
	c.Assert(fixture.nearest(c, "docs_emb") == 5, qt.Equals, caps[tablecreate.VectorMaintainedOnWriteCapability])

	// The plan names the key it lacks.
	bits := nativeIndexSet(nativeIndex(vectorIndex("docs_emb", `["gen","emb"]`, `["body"]`, 2), prefixedTables),
		nativeIndex(vectorIndexWith("docs_bits", `["emb"]`, "", `{"settings":{"metric":"DISTANCE_MANHATTAN","vectorType":"VECTOR_TYPE_BIT","vectorDimension":8},"levels":1,"clusters":2}`), vectorTables))
	found := fixture.compareIndexes(c, bits)
	without := proto.CloneOf(fixture.session)
	without.Capabilities[tablecreate.VectorBitTypeCapability] = false
	fixture.session = without
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, bits))
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `index "docs_bits" stores bit vectors, which requires target capability vector_bit_type, unavailable on this ydb target .*`)
	// The server's own answer, which the key names: the index built over the
	// rows above, or refused.
	built := fixture.exec(c, "ALTER TABLE "+fixture.recordsPath()+" ADD INDEX `docs_bits` GLOBAL USING vector_kmeans_tree ON (emb) "+
		"WITH (distance=manhattan, vector_type=bit, vector_dimension=8, levels=1, clusters=2)")
	c.Assert(built == nil, qt.Equals, caps[tablecreate.VectorBitTypeCapability], qt.Commentf("%v", built))
}

const (
	standardBody = `{"tokenizer":"STANDARD","useFilterLowercase":true}`
	keywordBody  = `{"tokenizer":"KEYWORD","useFilterLowercase":true}`
	plainKeyword = `{"tokenizer":"KEYWORD","useFilterLowercase":true,"useFilterStopwords":false}`
)

// TestFullTextIndexRoundTripSubprocess is the host's TestYDBFullText_RoundTrip
// on a line that builds full-text indexes: create, read back and compare a
// full-text index, then rebuild it for another tokenizer and as a plain index,
// each change a drop and an add, and nothing left to plan after each.
func TestFullTextIndexRoundTripSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newDocumentsFixture(c)
	requireSessionCapability(c, fixture.session, tablecreate.FullTextIndexesCapability)
	declared := nativeIndexSet(nativeIndex(fullTextIndex("globalFulltextRelevanceIndex", "ft", "body", standardBody), relevanceTables))
	plan := fixture.indexPlan(c, declared)
	_, statements := fixture.planned(c, plan)
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `<dir>/records` ADD INDEX `ft` GLOBAL USING fulltext_relevance ON (`body`) " +
		"WITH (tokenizer=standard, use_filter_lowercase=true);"})
	fixture.run(c, "fulltext-add", plan)
	c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
	analyzers := fixture.heldIndexes(c)["ft"].GetGlobalFulltextRelevanceIndex().GetFulltextSettings().GetColumns()[0].GetAnalyzers()
	c.Assert(analyzers.GetTokenizer(), qt.Equals, native.FulltextIndexSettings_STANDARD)
	c.Assert(analyzers.GetUseFilterLowercase(), qt.IsTrue)

	changed := nativeIndexSet(nativeIndex(fullTextIndex("globalFulltextRelevanceIndex", "ft", "body", keywordBody), relevanceTables))
	plan = fixture.indexPlan(c, changed)
	names, _ := fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-add-index/000000"})
	fixture.run(c, "fulltext-tokenizer", plan)
	c.Assert(fixture.compareIndexes(c, changed).Changes, qt.HasLen, 0)

	plain := nativeIndexSet(nativeIndex(fullTextIndex("globalFulltextPlainIndex", "ft", "body", plainKeyword), plainTables))
	plan = fixture.indexPlan(c, plain)
	names, _ = fixture.planned(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"row-table-drop-index/000000", "row-table-add-index/000000"})
	fixture.run(c, "fulltext-plain", plan)
	c.Assert(fixture.compareIndexes(c, plain).Changes, qt.HasLen, 0)
	held := fixture.heldIndexes(c)["ft"].GetGlobalFulltextPlainIndex().GetFulltextSettings().GetColumns()[0].GetAnalyzers()
	c.Assert(held.GetTokenizer(), qt.Equals, native.FulltextIndexSettings_KEYWORD)
	c.Assert(held.UseFilterStopwords, qt.IsNotNil)
	c.Assert(held.GetUseFilterStopwords(), qt.IsFalse)
}

// TestFullTextIndexOlderLineSubprocess is the host's
// TestYDBFullText_OlderLineRefuses: a line that predates the full-text grammar
// and its flag reports no full_text_indexes, the plan is refused before a
// statement is sent, and the server refuses the index itself.
func TestFullTextIndexOlderLineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newDocumentsFixture(c)
	requireNoSessionCapability(c, fixture.session, tablecreate.FullTextIndexesCapability)
	declared := nativeIndexSet(nativeIndex(fullTextIndex("globalFulltextRelevanceIndex", "ft", "body", standardBody), relevanceTables))
	found := fixture.compareIndexes(c, declared)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `index "ft" is a full-text index, which requires target capability full_text_indexes, unavailable on this ydb target .*`)
	err := fixture.exec(c, "CREATE TABLE `"+path.Join(fixture.database, fixture.directory, "absent")+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id), "+
		"INDEX ft GLOBAL USING fulltext_relevance ON (body) WITH (tokenizer=standard))")
	c.Assert(err, qt.ErrorMatches, `(?s).*FULLTEXT_RELEVANCE index subtype is not supported.*`)
}

// TestFullTextIndexDisabledFlagSubprocess is the host's
// TestYDBFullText_DisabledFlag: with EnableFulltextIndex turned off for the
// cluster, a new session reports no full_text_indexes, the plan is refused
// before a statement is sent, and the server refuses the index itself.
func TestFullTextIndexDisabledFlagSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newDocumentsFixture(c)
	requireSessionCapability(c, fixture.session, tablecreate.FullTextIndexesCapability)
	address := monitoredAddress(c)
	flags := newClusterFlags(c, fixture.connection(c))
	flags.set(c, map[string]bool{"EnableFulltextIndex": false})
	session, err := fixture.client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(fixture.client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities[tablecreate.FullTextIndexesCapability], qt.IsFalse)
	fixture.session = session
	declared := nativeIndexSet(nativeIndex(fullTextIndex("globalFulltextPlainIndex", "ft", "body", standardBody), plainTables))
	found := fixture.compareIndexes(c, declared)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `index "ft" is a full-text index, which requires target capability full_text_indexes, .*EnableFulltextIndex.*`)
	err = fixture.exec(c, "CREATE TABLE `"+path.Join(fixture.database, fixture.directory, "disabled")+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id), "+
		"INDEX ft GLOBAL USING fulltext_plain ON (body) WITH (tokenizer=standard))")
	c.Assert(err, qt.ErrorMatches, `(?s).*Fulltext index support is disabled.*`)
}
