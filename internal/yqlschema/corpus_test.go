package yqlschema_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// The corpus holds loads of YQL schema documents and what the Ptah host's
// bundled YDB owner made of each, recorded by testdata/oracle. A case is a
// directory of documents, read in name order as one load, with an optional
// <case>.load.json naming the database root, and <case>.golden.json holding
// the host's answer: its grammar's reading, which a provider's ReadSQLSchema
// replaces, and the coverage the load claims once the host's reader has read
// it. testdata/README.md says how the golden files are recorded.

const corpusDirectory = "testdata/corpus"

// golden is what the host recorded for one load.
type golden struct {
	Host         string        `json:"host"`
	DatabasePath string        `json:"database_path"`
	Documents    []string      `json:"documents"`
	Limits       []goldenLimit `json:"limits"`
	LimitsError  string        `json:"limits_error"`
	Grammar      goldenGrammar `json:"grammar"`
	Load         goldenLoad    `json:"load"`
}

// goldenLimit is one header entry the host sends a provider with the
// documents.
type goldenLimit struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
	Provenance string `json:"provenance"`
}

type goldenGrammar struct {
	Outcome        string                     `json:"outcome"`
	Error          string                     `json:"error"`
	Database       map[string]json.RawMessage `json:"database"`
	FeatureObjects []goldenObject             `json:"feature_objects"`
	TableFacets    []goldenFacets             `json:"table_facets"`
	FieldFacets    []goldenFacets             `json:"field_facets"`
	IndexFacets    []goldenFacets             `json:"index_facets"`
	Files          []goldenFile               `json:"files"`
}

type goldenLoad struct {
	Outcome  string          `json:"outcome"`
	Error    string          `json:"error"`
	Coverage *goldenCoverage `json:"coverage"`
}

type goldenObject struct {
	Ref   goldenRef      `json:"ref"`
	Value goldenEnvelope `json:"value"`
}

type goldenRef struct {
	Kind   string      `json:"kind"`
	Schema *goldenPart `json:"schema"`
	Parent *goldenPart `json:"parent"`
	Name   goldenPart  `json:"name"`
}

type goldenPart struct {
	Source string `json:"source"`
}

type goldenEnvelope struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type goldenFacets struct {
	Owner  string        `json:"owner"`
	Facets []goldenFacet `json:"facets"`
}

type goldenFacet struct {
	Kind  string          `json:"kind"`
	Value *goldenEnvelope `json:"value"`
}

type goldenFile struct {
	Path    string                     `json:"Path"`
	Objects []yqlschema.Declaration    `json:"Objects"`
	Guards  []yqlschema.Declaration    `json:"Guards"`
	Tables  []yqlschema.TableStatement `json:"Tables"`
}

type goldenCoverage struct {
	Kinds    []goldenKindCoverage    `json:"kinds"`
	Subjects []goldenSubjectCoverage `json:"subjects"`
}

type goldenKindCoverage struct {
	Model     goldenModel       `json:"model"`
	Knowledge []json.RawMessage `json:"knowledge"`
}

type goldenModel struct {
	Kind string `json:"kind"`
}

type goldenSubjectCoverage struct {
	Kind      string          `json:"kind"`
	Subject   goldenRef       `json:"subject"`
	Knowledge json.RawMessage `json:"knowledge"`
}

// row is one load of the corpus and the host's answer to it.
type row struct {
	name      string
	documents []yqlschema.Document
	golden    golden
}

// corpus lists the rows keep selects, in name order.
func corpus(c *qt.C, keep func(row) bool) []row {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDirectory, "*.golden.json"))
	c.Assert(err, qt.IsNil)
	var rows []row
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		data, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		var recorded golden
		c.Assert(json.Unmarshal(data, &recorded), qt.IsNil)
		r := row{name: name, golden: recorded}
		for _, document := range recorded.Documents {
			text, err := os.ReadFile(filepath.Join(corpusDirectory, name, document))
			c.Assert(err, qt.IsNil)
			r.documents = append(r.documents, yqlschema.Document{Path: document, Text: string(text)})
		}
		if keep(r) {
			rows = append(rows, r)
		}
	}
	return rows
}

// unsupportedRows are the cases this provider refuses by name, with the
// construct it names, from testdata/unsupported.txt.
func unsupportedRows(c *qt.C) map[string]string {
	c.Helper()
	data, err := os.ReadFile("testdata/unsupported.txt")
	c.Assert(err, qt.IsNil)
	listed := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		name, construct, found := strings.Cut(line, " ")
		if strings.HasPrefix(line, "#") || !found {
			continue
		}
		listed[name] = construct
	}
	return listed
}

func refused(r row) bool  { return r.golden.Grammar.Outcome == "refused" }
func accepted(r row) bool { return r.golden.Grammar.Outcome == "accepted" }

// hostRefusal is how a refusal of the host's grammar reads in the reply: the
// document it names, where it starts when the host names a position, and the
// message as this provider writes it.
type hostRefusal struct {
	Document int
	Offset   int
	Message  string
}

var (
	documentPrefix = regexp.MustCompile(`^(\d+)\.sql: `)
	positioned     = regexp.MustCompile(`^YQL schema at position (\d+): `)
)

// expectedRefusal reads a host refusal as this provider reports it. A
// refusal the host positions keeps its words after the position, and its
// offset; one it does not position has no offset to compare, and -1 stands
// for it.
func expectedRefusal(c *qt.C, message string) hostRefusal {
	c.Helper()
	document := documentPrefix.FindStringSubmatch(message)
	c.Assert(document, qt.HasLen, 2, qt.Commentf("%s", message))
	number, err := strconv.Atoi(document[1])
	c.Assert(err, qt.IsNil)
	rest := strings.TrimPrefix(message, document[0])
	result := hostRefusal{Document: number - 1, Offset: -1, Message: rest}
	if position := positioned.FindStringSubmatch(rest); position != nil {
		result.Offset, _ = strconv.Atoi(position[1])
		result.Message = "YQL schema: " + strings.TrimPrefix(rest, position[0])
	}
	result.Message = boundGot(c, result.Message)
	return result
}

// got is the token a host refusal quotes at its end.
var got = regexp.MustCompile(`\(got ("(?:[^"\\]|\\.)*")\)$`)

// boundGot writes the token a host refusal quotes as this provider quotes
// it: at most its first 64 bytes, cut at a character, and an ellipsis. A
// schema file can hold a secret in a string literal.
func boundGot(c *qt.C, message string) string {
	c.Helper()
	quoted := got.FindStringSubmatch(message)
	if quoted == nil {
		return message
	}
	token, err := strconv.Unquote(quoted[1])
	c.Assert(err, qt.IsNil)
	if len(token) <= 64 {
		return message
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(token[cut]) {
		cut--
	}
	return strings.TrimSuffix(message, quoted[0]) + "(got " + strconv.Quote(token[:cut]+"...") + ")"
}

// pluginRefusal is the provider's refusal in the shape of [hostRefusal],
// with the offset left out when the host names none.
func pluginRefusal(refusal *yqlschema.Refusal, want hostRefusal) hostRefusal {
	got := hostRefusal{Document: refusal.Document, Offset: refusal.Start, Message: refusal.Message}
	got.Offset = map[bool]int{true: -1, false: got.Offset}[want.Offset < 0]
	return got
}

// keys returns the keys of a JSON object in order.
func keys(object map[string]any) []string {
	return slices.Sorted(maps.Keys(object))
}
