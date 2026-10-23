package tablesource_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The corpus holds the inputs the host's bundled YDB source decoder
// (internal/ydbsource) was run over, and its answers. A yaml-*.json input is a
// whole YAML schema written as JSON, which YAML reads as flow syntax, so the
// host and this package read the same bytes. A go-*.go input is a Go file; its
// golden file also holds the declarations the host's frontend lexed from it,
// which are replayed here as the captured fragments the host would send.
// testdata/oracle explains how the golden files are regenerated.
//
// A file name says what the row asserts:
//
//	refused-*   the host refuses the input, and so must this package
//	deferred-*  the host accepts the input and the provider cannot carry it yet
//	any other   the host accepts the input, and both decoders mean the same
//	            partitioning, read replicas, bloom filter, TTL and column
//	            families

// golden is what the host recorded for one input.
type golden struct {
	Host         string              `json:"host"`
	Format       string              `json:"format"`
	Outcome      string              `json:"outcome"`
	Error        string              `json:"error"`
	Tables       []goldenTable       `json:"tables"`
	Declarations []goldenDeclaration `json:"declarations"`
}

type goldenTable struct {
	Schema string                     `json:"schema"`
	Name   string                     `json:"name"`
	Struct string                     `json:"struct"`
	Facets map[string]json.RawMessage `json:"facets"`
	// Indexes are the indexes whose options the host's decoder read.
	Indexes []json.RawMessage `json:"indexes"`
}

type goldenDeclaration struct {
	Directive  string            `json:"directive"`
	Attributes map[string]string `json:"attributes"`
	Struct     string            `json:"struct"`
	File       string            `json:"file"`
	FileSize   int               `json:"file_size"`
	Start      goldenPosition    `json:"start"`
	End        goldenPosition    `json:"end"`
	Subject    *goldenSubject    `json:"subject"`
}

type goldenSubject struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Table string `json:"table"`
}

type goldenPosition struct {
	Offset int `json:"offset"`
	Line   int `json:"line"`
	Column int `json:"column"`
}

// row is one corpus input and the host's answer to it.
type row struct {
	name   string
	input  string
	golden golden
}

const corpusDirectory = "testdata/corpus"

// corpus lists the rows whose name starts with prefix (or, for an empty
// prefix, every row that starts with none of the others), in name order.
func corpus(c *qt.C, keep func(name string) bool) []row {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDirectory, "*.golden.json"))
	c.Assert(err, qt.IsNil)
	var rows []row
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		if !keep(name) {
			continue
		}
		data, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		var recorded golden
		c.Assert(json.Unmarshal(data, &recorded), qt.IsNil)
		input, err := os.ReadFile(filepath.Join(corpusDirectory, inputName(name)))
		c.Assert(err, qt.IsNil)
		rows = append(rows, row{name: name, input: string(input), golden: recorded})
	}
	return rows
}

func inputName(name string) string {
	return name + map[bool]string{true: ".json", false: ".go"}[strings.HasPrefix(name, "yaml-")]
}

func isRefused(name string) bool  { return strings.Contains(name, "refused-") }
func isDeferred(name string) bool { return strings.Contains(name, "deferred-") }
func isAccepted(name string) bool { return !isRefused(name) && !isDeferred(name) }

// request replays a row as the captured document the host would send.
func request(c *qt.C, r row) *protocol.DecodeSourceRequest {
	c.Helper()
	if r.golden.Format == "yaml" {
		captured, err := testsource.YAMLTables(r.input)
		c.Assert(err, qt.IsNil)
		return captured
	}
	var tables []testsource.GoTable
	for _, table := range r.golden.Tables {
		tables = append(tables, testsource.GoTable{Schema: table.Schema, Name: table.Name, Struct: table.Struct})
	}
	var declarations []testsource.GoDeclaration
	for _, declaration := range r.golden.Declarations {
		replayed := testsource.GoDeclaration{Directive: declaration.Directive, Attributes: declaration.Attributes,
			Struct: declaration.Struct, File: declaration.File, FileSize: declaration.FileSize,
			Start: testsource.GoPosition(declaration.Start), End: testsource.GoPosition(declaration.End)}
		// The host sends a subject for a column or an index directive only; the
		// table directive's own subject is the table, which a fragment leaves out.
		if declaration.Subject != nil && declaration.Subject.Kind != "table" {
			replayed.Subject = testsource.GoSubject(*declaration.Subject)
		}
		declarations = append(declarations, replayed)
	}
	captured, err := testsource.GoDirectives(tables, declarations)
	c.Assert(err, qt.IsNil)
	return captured
}

// The corpus is the measurement, so it must not shrink without notice and every
// golden file must come from one run of one host commit.
func TestCorpusShape(t *testing.T) {
	c := qt.New(t)
	rows := corpus(c, func(string) bool { return true })
	c.Assert(len(rows) >= 135, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(rows)))
	hosts := map[string]bool{}
	for _, r := range rows {
		hosts[r.golden.Host] = true
		c.Assert(r.golden.Host, qt.Matches, `[0-9a-f]{40}`, qt.Commentf(r.name))
	}
	c.Assert(slices.Collect(maps.Keys(hosts)), qt.HasLen, 1)
	for _, r := range rows {
		c.Assert(r.golden.Outcome == "refused", qt.Equals, isRefused(r.name), qt.Commentf("%s: host outcome %q", r.name, r.golden.Outcome))
	}
	c.Assert(len(corpus(c, isRefused)) >= 60, qt.IsTrue)
	c.Assert(len(corpus(c, isDeferred)) >= 18, qt.IsTrue)
	c.Assert(len(corpus(c, isAccepted)) >= 50, qt.IsTrue)
}

// TestDifferential_HappyPath decodes every input the host accepts and requires
// the facets to mean what the host's facets mean, table by table.
func TestDifferential_HappyPath(t *testing.T) {
	rows := corpus(qt.New(t), isAccepted)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			decoded, err := backend.New().DecodeSource(c.Context(), request(c, r))
			c.Assert(err, qt.IsNil)
			c.Assert(pluginMeaning(c, decoded), qt.DeepEquals, hostMeaning(c, r.golden), qt.Commentf("host %s", r.golden.Host))
		})
	}
}

// TestDifferential_FailurePath requires the provider to refuse every input the
// host refuses, and to return no partial result.
func TestDifferential_FailurePath(t *testing.T) {
	rows := corpus(qt.New(t), isRefused)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			decoded, err := backend.New().DecodeSource(c.Context(), request(c, r))
			c.Assert(err, qt.IsNotNil, qt.Commentf("the host refuses it with: %s", r.golden.Error))
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// unclaimed names, for every input the host accepts and the provider cannot
// carry yet, the YAML key the provider does not claim. The host refuses an
// unclaimed YAML key by name before it dispatches anything. (It reads an
// unclaimed Go comment as no declaration at all, so the provider claims the
// Go directives it cannot carry and refuses them itself.)
var unclaimed = map[string]string{
	"yaml-deferred-uniform-partitions": "uniform_partitions",
	"yaml-deferred-partition-at-keys":  "partition_at_keys",
	"yaml-deferred-changefeeds":        "changefeeds",
	"yaml-deferred-column-store":       "column_store",
	"yaml-deferred-ttl-platform":       "platform",
}

// refusals names the inputs whose key or attribute the provider claims and
// refuses by name, with the words of the refusal.
var refusals = map[string]string{
	"yaml-deferred-family-cache-mode":      "cache_mode is not supported by the YDB provider yet",
	"yaml-deferred-family-fields":          "fields is not supported by the YDB provider yet",
	"go-deferred-family-cache-mode":        "cache_mode is not supported by the YDB provider yet",
	"go-deferred-family-fields":            "fields is not supported by the YDB provider yet",
	"go-deferred-changefeed":               "ptah:schema:changefeed is not supported by the YDB provider yet",
	"go-deferred-changefeed-consumer":      "ptah:schema:changefeed is not supported by the YDB provider yet",
	"go-deferred-table-uniform-partitions": "uniform_partitions is not supported by the YDB provider yet",
	"go-deferred-table-partition-at-keys":  "partition_at_keys is not supported by the YDB provider yet",
	"go-deferred-table-store-column":       "store is not supported by the YDB provider yet",
	"go-deferred-table-store-column-hash":  "store is not supported by the YDB provider yet",
	"go-deferred-index-partitioning":       "auto_partitioning_by_load is not supported by the YDB provider yet",
	"go-deferred-index-replicas":           "read_replicas_settings is not supported by the YDB provider yet",
	"go-deferred-index-vector":             "clusters is not supported by the YDB provider yet",
	"go-deferred-index-fulltext":           "tokenizer is not supported by the YDB provider yet",
	"go-deferred-index-bloom":              "false_positive_probability is not supported by the YDB provider yet",
}

// TestDeferred_AreAllNamed requires every deferred row to say what the
// provider does with it, and no statement to name a row that is gone.
func TestDeferred_AreAllNamed(t *testing.T) {
	c := qt.New(t)
	var rows []string
	for _, r := range corpus(c, isDeferred) {
		rows = append(rows, r.name)
		c.Assert(r.golden.Outcome, qt.Equals, "accepted", qt.Commentf(r.name))
	}
	named := slices.Concat(slices.Collect(maps.Keys(unclaimed)), slices.Collect(maps.Keys(refusals)))
	c.Assert(slices.Sorted(slices.Values(rows)), qt.DeepEquals, slices.Sorted(slices.Values(named)))
}

// TestDeferred_Unclaimed requires the key or directive to stay unclaimed, so
// the host refuses it instead of the provider dropping it.
func TestDeferred_Unclaimed(t *testing.T) {
	for name, key := range unclaimed {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(claimedNames()[key], qt.IsFalse)
		})
	}
}

// TestDeferred_RefusedByName decodes the inputs whose key or attribute the
// provider claims and cannot carry yet, and requires the refusal to name it.
func TestDeferred_RefusedByName(t *testing.T) {
	for name, words := range refusals {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			rows := corpus(c, func(candidate string) bool { return candidate == name })
			c.Assert(len(rows), qt.Equals, 1)
			decoded, err := backend.New().DecodeSource(c.Context(), request(c, rows[0]))
			c.Assert(err, qt.ErrorMatches, `(?s).*`+words+`.*`)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

func claimedNames() map[string]bool {
	names := map[string]bool{}
	for _, key := range tablesource.YAMLKeys() {
		names[key.Key] = true
	}
	for _, directive := range tablesource.GoDirectives() {
		names[directive.Directive] = true
	}
	return names
}
