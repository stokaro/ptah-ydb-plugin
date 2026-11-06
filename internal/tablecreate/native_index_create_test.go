package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// documentsCreation plans the creation of app.records, a table of documents
// with a generation, a vector, a body and a title, the common indexes given
// and the native index set indexes, on a target that builds every vector and
// full-text index.
func documentsCreation(c *qt.C, indexes string, common ...*protocol.DeclaredIndex) *protocol.PlanFeaturesRequest {
	c.Helper()
	table := &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "gen", Type: "Int64", Nullable: true},
			{Name: "emb", Type: "String", Nullable: true}, {Name: "body", Type: "Utf8", Nullable: true}, {Name: "title", Type: "Utf8", Nullable: true}},
		Indexes: common,
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
				Records: []*protocol.ComparisonRecord{facet(c, schema.IndexProperties, "", indexes)}}}}
	capabilities := everyIndexKind()
	capabilities["transactional_ddl"] = false
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, DatabasePath: "/local", ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Capabilities: capabilities,
		Tables:       []*protocol.PlanningTable{{Subject: tableSubject(), Action: protocol.PlanningTable_CREATE, Desired: table}}}
}

// A new table's vector and full-text indexes are built by its creation, as
// the host's embedded engine at 74de0b65b built them: one operation, whose
// CREATE TABLE writes every index of the table in name order, the global ones
// among them. The creation's effects create each index.
func TestNativeIndexCreation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := documentsCreation(c, indexSet(nativeDeclared(docsEmb, prefixedTables), nativeDeclared(standardBody, relevanceTables)),
		&protocol.DeclaredIndex{Name: "by_title", Fields: []string{"title"}})

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plannedSteps(plan), qt.DeepEquals, []string{"row-table-create/000000 row-table-create-operation"})
	c.Assert(edges(plan), qt.HasLen, 0)
	var effects []string
	for _, effect := range plan.Contributions[0].Steps[0].Metadata.Effects {
		effects = append(effects, effect.Action.String()+" "+effect.Subject.Kind+" "+effect.Subject.Name.Source)
	}
	c.Assert(effects, qt.DeepEquals, []string{"CREATE table records", "CREATE ydb.scheme-path records",
		"CREATE index by_title", "CREATE index docs_emb", "CREATE index ft"})
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 1)
	}
	c.Assert(textUnits(c, plan, request.Capabilities), qt.DeepEquals, []string{
		"CREATE TABLE `app/records` (\n    `id` Uint64 NOT NULL,\n    `gen` Int64,\n    `emb` String,\n    `body` Utf8,\n    `title` Utf8,\n" +
			"    PRIMARY KEY (`id`),\n    INDEX `by_title` GLOBAL SYNC ON (`title`),\n" +
			"    INDEX `docs_emb` GLOBAL USING vector_kmeans_tree ON (`gen`, `emb`) COVER (`body`) WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2),\n" +
			"    INDEX `ft` GLOBAL USING fulltext_relevance ON (`body`) WITH (tokenizer=standard, use_filter_lowercase=true)\n);",
	})
}

// The creation carries a vector or full-text index through its payload and
// back: the operation a host returns normalizes to the same request, whose
// index holds the vector arm the pinned bindings keep as an unknown field.
func TestNativeIndexCreation_RoundTrip(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), documentsCreation(c, indexSet(nativeDeclared(docsEmb, prefixedTables))))
	c.Assert(err, qt.IsNil)
	value := plan.Contributions[0].Steps[0].Operation.Value.Value

	normalized, err := tablecreate.Normalize(&protocol.ValueInput{Id: "again", Representation: protocol.ValueInput_OPERATION, Value: value})

	c.Assert(err, qt.IsNil)
	c.Assert(string(normalized.Value.Payload), qt.Equals, string(value.Payload))
	created, err := tablecreate.Request(normalized.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(created.Indexes, qt.HasLen, 1)
	spec, err := schema.NativeRequestIndex(created.Indexes[0])
	c.Assert(err, qt.IsNil)
	c.Assert(spec.GetGlobalVectorKmeansTreeIndex().GetVectorSettings().GetClusters(), qt.Equals, uint32(2))
}

func TestNativeIndexCreation_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		indexes string
		common  []*protocol.DeclaredIndex
		message string
	}{
		{"a global index in the native index set", indexSet(nativeDeclared(indexJSON("globalIndex", "by_title", []string{"title"}, nil), plainTables)), nil,
			`index "by_title": the native index set states a global index, and a new table takes its global indexes from the common declaration`},
		{"a vector index under the name of a common index", indexSet(nativeDeclared(docsEmb, prefixedTables)), []*protocol.DeclaredIndex{{Name: "docs_emb", Fields: []string{"title"}}},
			`table "app.records": two of its indexes are named "docs_emb", and YDB names an index once per table`},
		{"a vector index on a column the table does not declare", indexSet(nativeDeclared(vectorJSON("v", []string{"other"}, nil, cosine3(2), ""), vectorTables)), nil,
			`index "v" reads column "other", which the declaration does not state`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), documentsCreation(c, test.indexes, test.common...))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// A creation on a target without vector indexes is refused before the table
// is created, so no table is left without the index its declaration states.
func TestNativeIndexCreation_RefusedWithoutTheCapability(t *testing.T) {
	c := testcheck.New(t)
	request := documentsCreation(c, indexSet(nativeDeclared(docsEmb, prefixedTables)))
	request.Capabilities[tablecreate.VectorIndexesCapability] = false
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `index "docs_emb" declares vector settings, which requires target capability vector_indexes, .*`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
