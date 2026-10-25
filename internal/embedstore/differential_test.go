package embedstore_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/embedstore"
)

// testdata/golden.json holds what the Ptah host's embedded YDB engine
// (internal/embedydb) answers over the corpus in testdata, recorded by
// testdata/oracle at one host commit: the run-state tables it creates on 25.1
// and on 26.2, the objects each specification of targets.json needs or the
// reason YDB cannot hold it, and the bytes each vector of vectors.json is
// stored as. The tests hold the provider to the same answers, so a database
// either of them writes is one the other reads.

type relation struct {
	Schema         string            `json:"schema"`
	Table          string            `json:"table"`
	Layout         string            `json:"layout"`
	Representation string            `json:"representation"`
	Metric         string            `json:"metric"`
	IndexMethod    string            `json:"index_method"`
	IndexOptions   map[string]string `json:"index_options"`
}

type targetCase struct {
	Name      string   `json:"name"`
	Dimension uint32   `json:"dimension"`
	Chunked   bool     `json:"chunked"`
	Target    relation `json:"target"`
}

type objects struct {
	Name          string `json:"name"`
	Refused       bool   `json:"refused"`
	Message       string `json:"message"`
	ColumnType    string `json:"column_type"`
	OwnsTable     bool   `json:"owns_table"`
	HasIndex      bool   `json:"has_index"`
	IndexMethod   string `json:"index_method"`
	IndexOperator string `json:"index_operator"`
}

type golden struct {
	HostCommit string              `json:"host_commit"`
	Schema     map[string][]string `json:"schema"`
	Targets    []objects           `json:"targets"`
	Vectors    []string            `json:"vectors"`
}

func readJSON(c *qt.C, file string, target any) {
	c.Helper()
	data, err := os.ReadFile(file)
	c.Assert(err, qt.IsNil)
	c.Assert(json.Unmarshal(data, target), qt.IsNil)
}

func hostAnswers(c *qt.C) golden {
	c.Helper()
	var answers golden
	readJSON(c, "testdata/golden.json", &answers)
	c.Assert(answers.HostCommit, qt.Matches, `[0-9a-f]{40}`)
	return answers
}

var metrics = map[string]protocol.EmbeddingTarget_Metric{
	"cosine": protocol.EmbeddingTarget_COSINE, "l2": protocol.EmbeddingTarget_L2, "inner_product": protocol.EmbeddingTarget_INNER_PRODUCT,
}

var layouts = map[string]protocol.EmbeddingTarget_Layout{
	"": protocol.EmbeddingTarget_SOURCE_COLUMNS, "own_table": protocol.EmbeddingTarget_OWN_TABLE,
}

// spec is a case as the host sends it, with the oracle's defaults: a source
// of app/articles keyed by id, and a float target in app/articles.
func spec(test targetCase) *protocol.EmbeddingSpec {
	target := test.Target
	return &protocol.EmbeddingSpec{
		Generation: "0123456789abcdef", Chunked: test.Chunked,
		Source: &protocol.EmbeddingSource{Table: &protocol.Reference{Kind: "table", Schema: "app", Name: "articles"},
			KeyFields: []string{"id"}, InputFields: []string{"title"}},
		Target: &protocol.EmbeddingTarget{Table: &protocol.Reference{Kind: "table", Schema: orDefault(target.Schema, "app"), Name: orDefault(target.Table, "articles")},
			Column: "embedding", Layout: layouts[target.Layout], Representation: orDefault(target.Representation, "float"),
			Metric: metrics[target.Metric], Dimension: test.Dimension, IndexMethod: target.IndexMethod, IndexOptions: target.IndexOptions},
		Names: &protocol.EmbeddingNames{Index: "articles_embedding_0123456789ab_idx", OwnershipMark: "mark"},
	}
}

// orDefault is value, or fallback where value is empty.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func TestSchemaStatementsAreTheHosts(t *testing.T) {
	answers := hostAnswers(qt.New(t))
	for _, test := range []struct {
		line string
		wide bool
	}{{"25.1", false}, {"26.2", true}} {
		t.Run(test.line, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(embedstore.SchemaStatements(test.wide), qt.DeepEquals, answers.Schema[test.line])
		})
	}
}

func TestDescribeTargetAnswersAsTheHost(t *testing.T) {
	c := qt.New(t)
	answers := hostAnswers(c)
	var cases []targetCase
	readJSON(c, "testdata/targets.json", &cases)
	c.Assert(cases, qt.HasLen, len(answers.Targets))
	c.Assert(len(cases) >= 20, qt.IsTrue)
	for i, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			c := qt.New(t)
			want := answers.Targets[i]

			got := embedstore.DescribeTarget(spec(test))

			c.Assert(want.Name, qt.Equals, test.Name)
			c.Assert(got.GetRefusal() != nil, qt.Equals, want.Refused)
			c.Assert(want.Message, qt.Contains, got.GetRefusal().GetMessage())
			c.Assert([]any{got.GetColumnType(), got.GetOwnsTable(), got.GetHasIndex(), got.GetIndexMethod(), got.GetIndexOperator()},
				qt.DeepEquals, []any{want.ColumnType, want.OwnsTable, want.HasIndex, want.IndexMethod, want.IndexOperator})
		})
	}
}

func TestEncodeVectorStoresTheHostsBytes(t *testing.T) {
	c := qt.New(t)
	answers := hostAnswers(c)
	var vectors [][]float32
	readJSON(c, "testdata/vectors.json", &vectors)
	c.Assert(vectors, qt.HasLen, len(answers.Vectors))
	for i, vector := range vectors {
		c.Assert(hex.EncodeToString(embedstore.EncodeVector(vector)), qt.Equals, answers.Vectors[i])
	}
}

// The corpus reaches both kinds of answer and every reason a target is
// refused for.
func TestEmbeddingCorpusReachesEveryAnswer(t *testing.T) {
	c := qt.New(t)
	refused, accepted := 0, 0
	for _, answer := range hostAnswers(c).Targets {
		refused += map[bool]int{true: 1}[answer.Refused]
		accepted += map[bool]int{false: 1}[answer.Refused]
	}
	c.Assert(refused > 10, qt.IsTrue)
	c.Assert(accepted > 5, qt.IsTrue)
}
