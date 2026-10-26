package tablecreate_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexedTable declares app.records with a key, a mail, a note and a score
// column, and the given indexes.
func indexedTable(indexes ...*protocol.DeclaredIndex) *protocol.DeclaredTable {
	return &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "mail", Type: "Utf8", Nullable: true},
			{Name: "note", Type: "Utf8", Nullable: true}, {Name: "score", Type: "Double", Nullable: true}},
		Indexes: indexes}
}

func TestCompileDeclaredIndexes_HappyPath(t *testing.T) {
	c := qt.New(t)
	request, err := tablecreate.Compile(owner(), indexedTable(
		&protocol.DeclaredIndex{Name: "by_mail", Fields: []string{"mail"}, Unique: true},
		&protocol.DeclaredIndex{Name: "by_note", Parts: []*protocol.CapturedIndexPart{{Column: "note"}, {Column: "id"}}, Type: "GLOBAL ASYNC"},
		&protocol.DeclaredIndex{Name: "by_mail_note", Fields: []string{"mail"}, IncludeColumns: []string{"note", "score"}, Type: "btree", TableName: "app.records"},
	), nil)
	c.Assert(err, qt.IsNil)
	want := []*Ydb_Table.TableIndex{
		{Name: "by_mail", IndexColumns: []string{"mail"}, Type: &Ydb_Table.TableIndex_GlobalUniqueIndex{GlobalUniqueIndex: &Ydb_Table.GlobalUniqueIndex{}}},
		{Name: "by_note", IndexColumns: []string{"note", "id"}, Type: &Ydb_Table.TableIndex_GlobalAsyncIndex{GlobalAsyncIndex: &Ydb_Table.GlobalAsyncIndex{}}},
		{Name: "by_mail_note", IndexColumns: []string{"mail"}, DataColumns: []string{"note", "score"}, Type: &Ydb_Table.TableIndex_GlobalIndex{GlobalIndex: &Ydb_Table.GlobalIndex{}}},
	}
	c.Assert(request.Indexes, qt.HasLen, len(want))
	for i, index := range want {
		c.Assert(proto.Equal(request.Indexes[i], index), qt.IsTrue, qt.Commentf("%v", request.Indexes[i]))
	}
	statement, err := tablecreate.RowTableStatement("app/records", request)
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, "CREATE TABLE `app/records` (\n    `id` Uint64 NOT NULL,\n    `mail` Utf8,\n    `note` Utf8,\n    `score` Double,\n"+
		"    PRIMARY KEY (`id`),\n    INDEX `by_mail` GLOBAL UNIQUE SYNC ON (`mail`),\n    INDEX `by_note` GLOBAL ASYNC ON (`note`, `id`),\n"+
		"    INDEX `by_mail_note` GLOBAL SYNC ON (`mail`) COVER (`note`, `score`)\n);")
}

// Each row is an index YDB has no clause for or refuses, and the words the
// refusal names it with, which are the host's embedded engine's.
func TestCompileDeclaredIndexes_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index *protocol.DeclaredIndex
		want  string
	}{
		{name: "no name", index: &protocol.DeclaredIndex{Fields: []string{"mail"}},
			want: `an index on table "records": YDB names every index, and this one has no name`},
		{name: "a unique async index", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, Unique: true, Type: "async"},
			want: "a unique index is synchronous on YDB"},
		{name: "a vector index", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, Type: "vector_kmeans_tree"},
			want: "it is a vector_kmeans_tree index, and this provider creates a table with its global indexes, synchronous or asynchronous"},
		{name: "pgvector's method", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, Type: "hnsw"},
			want: `index method "hnsw" is pgvector's and has no YDB counterpart`},
		{name: "the table's key", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"id"}},
			want: "its columns are the table's key, which YDB refuses (`index keys shouldn't be table keys`)"},
		{name: "a covered key column", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, IncludeColumns: []string{"id"}},
			want: `it covers key column "id", which every YDB index carries already`},
		{name: "a key that is covered too", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, IncludeColumns: []string{"mail"}},
			want: `column "mail" is both a key and a covered column, which YDB refuses`},
		{name: "an unknown column", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"ghost"}},
			want: `it names column "ghost", which the table does not declare`},
		{name: "an unknown covered column", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, IncludeColumns: []string{"ghost"}},
			want: `it covers column "ghost", which the table does not declare`},
		{name: "a column twice", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail", "mail"}}, want: `it names column "mail" twice`},
		{name: "a Double key", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"score"}},
			want: `column "score" is Double, which YDB refuses as an index key`},
		{name: "an expression", index: &protocol.DeclaredIndex{Name: "i", Parts: []*protocol.CapturedIndexPart{{Expression: "lower(mail)"}}},
			want: "YDB has no expression index"},
		{name: "an order", index: &protocol.DeclaredIndex{Name: "i", Parts: []*protocol.CapturedIndexPart{{Column: "mail", Descending: true}}},
			want: "a YDB index column has no order"},
		{name: "a prefix", index: &protocol.DeclaredIndex{Name: "i", Parts: []*protocol.CapturedIndexPart{{Column: "mail", Prefix: "8"}}},
			want: "a YDB index column has no prefix length"},
		{name: "a condition", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, Condition: "mail <> ''"},
			want: "YDB table creation does not support ptah.provider.experimental.DeclaredIndex.condition"},
		{name: "another table", index: &protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, TableName: "other"},
			want: `it names table "other", and the table declaring it is "records"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := tablecreate.Compile(owner(), indexedTable(tc.index), nil)
			c.Assert(err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(tc.want)+".*")
			c.Assert(request, qt.IsNil)
		})
	}
	c := qt.New(t)
	request, err := tablecreate.Compile(owner(), indexedTable(&protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}},
		&protocol.DeclaredIndex{Name: "i", Fields: []string{"note"}}), nil)
	c.Assert(err, qt.ErrorMatches, `.*two of its indexes are named "i".*`)
	c.Assert(request, qt.IsNil)
}

// A creation with indexes is one step whose effects create the table and each
// index, and it takes the host's step that creates both.
func TestCreationPlanCreatesTheIndexes(t *testing.T) {
	c := qt.New(t)
	request := creationPlan(c)
	table := request.Tables[0]
	table.Desired.Features.Facets.Records = []*protocol.ComparisonRecord{
		facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"]}`)}
	table.Desired.Indexes = []*protocol.DeclaredIndex{{Name: "by_value", Fields: []string{"value"}}}
	index := proto.CloneOf(table.Subject)
	index.Kind, index.Parent, index.Name = "index", proto.CloneOf(table.Subject.Name), part("by_value")
	request.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: proto.CloneOf(table.Subject),
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(table.Subject), Action: protocol.PlanningEffect_CREATE},
				{Subject: index, Action: protocol.PlanningEffect_CREATE}}}}}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	effects := plan.Contributions[0].Steps[0].Metadata.Effects
	c.Assert(effects, qt.HasLen, 3)
	c.Assert(proto.Equal(effects[2].Subject, index), qt.IsTrue)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/records` (\n    `first` Uint64 NOT NULL,\n    `second` Utf8 NOT NULL,\n"+
		"    `value` Decimal(22,9),\n    PRIMARY KEY (`second`, `first`),\n    INDEX `by_value` GLOBAL SYNC ON (`value`)\n);")

	// A host step that also touches another table's index is not a creation
	// this service takes whole.
	other := proto.CloneOf(index)
	other.Parent = part("elsewhere")
	request.CommonSteps[0].Metadata.Effects[1].Subject = other
	plan, err = backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, "A table creation rewrite requires one complete common table footprint")
}
