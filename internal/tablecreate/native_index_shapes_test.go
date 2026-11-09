package tablecreate_test

import (
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// declaredAs is documents, the table of documents, with the columns named in
// types declared as written there, and the index set desired.
func declaredAs(types map[string]string, desired string) alterCase {
	in := documents(desired, indexSet())
	maps.Copy(in.types, types)
	return in
}

// keyedCreation is documentsCreation with the vector column declared as
// vectorType and the table keyed by the column key, which every other column
// is nullable beside.
func keyedCreation(c *qt.C, vectorType, key, indexes string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := documentsCreation(c, indexes)
	table := request.Tables[0].Desired
	table.PrimaryKey = []string{key}
	for _, column := range table.Columns {
		column.Primary, column.Nullable = column.Name == key, column.Name != key
		if column.Name == "emb" {
			column.Type = vectorType
		}
	}
	return request
}

// A vector or full-text index reads each column as the type the table is
// created with, so an index over a column declared VECTOR(n) or TEXT is added
// to a table that exists. A String column declared without a dimension takes
// a vector index of any dimension.
//
// Ported from the host's TestShapeRefusal_Vector and
// TestGenerateMigrationAST_VectorIndex_HappyPath.
func TestNativeIndexShapes_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		in    alterCase
		steps []string
		sql   []string
	}{
		{"a VECTOR(3) column under an index of dimension 3", declaredAs(map[string]string{"emb": "VECTOR(3)"}, indexSet(nativeDeclared(docsEmb, prefixedTables))),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"}, []string{addDocsEmbSQL}},
		{"a String column declared without a dimension", declaredAs(nil, indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil,
			kmeans("DISTANCE_COSINE", "VECTOR_TYPE_FLOAT", 1536, 1, 2), ""), vectorTables))),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"ALTER TABLE `app/records` ADD INDEX `v` GLOBAL USING vector_kmeans_tree ON (`emb`) " +
				"WITH (distance=cosine, vector_type=float, vector_dimension=1536, levels=1, clusters=2);"}},
		{"a full-text index over a column declared TEXT", declaredAs(map[string]string{"body": "TEXT"},
			indexSet(nativeDeclared(fullTextJSON("globalFulltextPlainIndex", "ft", "body", `{"tokenizer":"STANDARD"}`), plainTables))),
			[]string{"row-table-add-index/000000 row-table-native-index-operation"},
			[]string{"ALTER TABLE `app/records` ADD INDEX `ft` GLOBAL USING fulltext_plain ON (`body`) WITH (tokenizer=standard);"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, test.in)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plannedSteps(plan), qt.DeepEquals, test.steps)
			c.Assert(textUnits(c, plan, request.Capabilities), qt.DeepEquals, test.sql)
		})
	}
}

// A vector index added to a table that exists is refused when its vector
// column is declared with another dimension, which YDB does not check, and
// when its last column, the one it reads vectors from, is a key column of
// another type than String.
//
// Ported from the host's TestShapeRefusal_Vector and
// TestGenerateMigrationAST_VectorIndex_FailurePath.
func TestNativeIndexShapes_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"a vector column declared with another dimension", declaredAs(map[string]string{"emb": "VECTOR(4)"}, indexSet(nativeDeclared(docsEmb, prefixedTables))),
			`index "docs_emb": its vector column "emb" is declared with dimension 4 and the index with vector_dimension 3; ` +
				`YDB checks neither, and leaves a vector of the wrong length out of the index`},
		{"a key column last", declaredAs(nil, indexSet(nativeDeclared(vectorJSON("v", []string{"emb", "id"}, nil, cosine3(2), ""), prefixedTables))),
			`index "v": its vector column "id" is Uint64, and a YDB vector index reads a String column \(.Embedding column 'id' expected type 'String'.\)`},
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

// A new table's vector index is built over a VECTOR(n) column of the index's
// dimension, and over a String column that is the table's key, which YDB
// takes for a vector index.
//
// Ported from the host's TestShapeRefusal_Vector.
func TestNativeIndexShapesOnCreation_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, vectorType, key, indexes string
		sql                            []string
	}{
		{"a VECTOR(3) column under an index of dimension 3", "VECTOR(3)", "id", indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, cosine3(2), ""), vectorTables)),
			[]string{"CREATE TABLE `app/records` (\n    `id` Uint64 NOT NULL,\n    `gen` Int64,\n    `emb` String,\n    `body` Utf8,\n    `title` Utf8,\n" +
				"    PRIMARY KEY (`id`),\n" +
				"    INDEX `v` GLOBAL USING vector_kmeans_tree ON (`emb`) WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2)\n);"}},
		{"a vector column that is the table's key", "String", "emb", indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, cosine3(2), ""), vectorTables)),
			[]string{"CREATE TABLE `app/records` (\n    `id` Uint64,\n    `gen` Int64,\n    `emb` String NOT NULL,\n    `body` Utf8,\n    `title` Utf8,\n" +
				"    PRIMARY KEY (`emb`),\n" +
				"    INDEX `v` GLOBAL USING vector_kmeans_tree ON (`emb`) WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2)\n);"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := keyedCreation(c, test.vectorType, test.key, test.indexes)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plannedSteps(plan), qt.DeepEquals, []string{"row-table-create/000000 row-table-create-operation"})
			c.Assert(textUnits(c, plan, request.Capabilities), qt.DeepEquals, test.sql)
		})
	}
}

// A new table's vector index is refused when its vector column is declared
// with another dimension, as on a table that exists.
//
// Ported from the host's TestShapeRefusal_Vector.
func TestNativeIndexShapesOnCreation_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), keyedCreation(c, "VECTOR(4)", "id",
		indexSet(nativeDeclared(vectorJSON("v", []string{"emb"}, nil, cosine3(2), ""), vectorTables))))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `index "v": its vector column "emb" is declared with dimension 4 and the index with vector_dimension 3; `+
		`YDB checks neither, and leaves a vector of the wrong length out of the index`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
