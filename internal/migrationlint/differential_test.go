package migrationlint_test

import (
	"cmp"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// testdata/corpus holds migration directories and requests, and the answers
// the Ptah host's bundled YDB lint owner gave for them at one host commit;
// testdata/README.md says how they are recorded. Every split, annotation and
// check the host sent the owner is replayed here, and the provider has to
// answer it as the host did: the same statements with the same words,
// comments and effects, the same annotations, and the same findings with the
// same messages and subjects, in the order the host reports them.

// statement is a statement in the form the golden files write it.
type statement struct {
	Start       int      `json:"start"`
	End         int      `json:"end"`
	Line        int      `json:"line"`
	SQL         string   `json:"sql"`
	Canonical   string   `json:"canonical"`
	Words       []string `json:"words,omitempty"`
	SourceWords []string `json:"source_words,omitempty"`
	Comments    []string `json:"comments,omitempty"`
	Effects     []effect `json:"effects,omitempty"`
}

type effect struct {
	Fact    string   `json:"fact"`
	Object  string   `json:"object,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Table   string   `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
	Clause  string   `json:"clause,omitempty"`
	Failure string   `json:"failure,omitempty"`
	Impact  string   `json:"impact,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

type source struct {
	Name       string `json:"name"`
	SQL        string `json:"sql"`
	Version    int64  `json:"version,omitempty"`
	Up         bool   `json:"up,omitempty"`
	Down       bool   `json:"down,omitempty"`
	Repeatable bool   `json:"repeatable,omitempty"`
}

type finding struct {
	Code      string    `json:"code"`
	File      int       `json:"file"`
	Statement int       `json:"statement"`
	Message   string    `json:"message"`
	Subjects  []subject `json:"subjects,omitempty"`
}

type subject struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Parent   string `json:"parent,omitempty"`
	DataType string `json:"data_type,omitempty"`
}

type annotation struct {
	Effects  []effect `json:"effects,omitempty"`
	Declined bool     `json:"declined,omitempty"`
}

type splitCall struct {
	Sources []source      `json:"sources"`
	Files   [][]statement `json:"files"`
	Judged  []string      `json:"judged"`
}

type annotateCall struct {
	Facts     []string       `json:"facts"`
	SourcesOf int            `json:"sources_of"`
	Sources   []source       `json:"sources"`
	Files     [][]statement  `json:"files"`
	Answer    [][]annotation `json:"answer"`
}

type checkFile struct {
	Name          string      `json:"name"`
	Version       int64       `json:"version"`
	Up            bool        `json:"up"`
	Down          bool        `json:"down"`
	HasPair       bool        `json:"has_pair"`
	NoTransaction bool        `json:"no_transaction"`
	Repeatable    bool        `json:"repeatable"`
	Source        int         `json:"source"`
	LineOffset    int         `json:"line_offset"`
	Statements    []statement `json:"statements"`
}

type checkCall struct {
	ServerVersion string          `json:"server_version"`
	Overrides     map[string]bool `json:"overrides"`
	Compatibility string          `json:"compatibility"`
	Codes         []string        `json:"codes"`
	Files         []checkFile     `json:"files"`
	Findings      []finding       `json:"findings"`
}

type run struct {
	Dialect       string        `json:"dialect"`
	ServerVersion string        `json:"server_version"`
	Compatibility string        `json:"compatibility"`
	Split         int           `json:"split"`
	Annotate      *annotateCall `json:"annotate"`
	Check         *checkCall    `json:"check"`
}

type directoryGolden struct {
	Host   string      `json:"host"`
	Splits []splitCall `json:"splits"`
	Runs   []run       `json:"runs"`
}

type requestGolden struct {
	Host   string          `json:"host"`
	Kind   string          `json:"kind"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result"`
}

const testdata = "testdata"

func readJSON(c *qt.C, path string, value any) {
	c.Helper()
	data, err := os.ReadFile(path)
	c.Assert(err, qt.IsNil)
	c.Assert(json.Unmarshal(data, value), qt.IsNil, qt.Commentf(path))
}

// directoryCase is one migration directory of the corpus and the host's
// answers for it.
type directoryCase struct {
	name   string
	golden directoryGolden
}

func directoryCases(c *qt.C) []directoryCase {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join(testdata, "corpus", "*.golden.json"))
	c.Assert(err, qt.IsNil)
	var cases []directoryCase
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		if info, err := os.Stat(filepath.Join(testdata, "corpus", name)); err != nil || !info.IsDir() {
			continue
		}
		var golden directoryGolden
		readJSON(c, path, &golden)
		cases = append(cases, directoryCase{name: name, golden: golden})
	}
	return cases
}

// requestCase is one request of the corpus and the host's answer to it.
type requestCase struct {
	name    string
	kind    string
	request json.RawMessage
	golden  requestGolden
}

func requestCases(c *qt.C, kind string) []requestCase {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join(testdata, "corpus", "*.request.json"))
	c.Assert(err, qt.IsNil)
	var cases []requestCase
	for _, path := range paths {
		var input struct {
			Kind    string          `json:"kind"`
			Request json.RawMessage `json:"request"`
		}
		readJSON(c, path, &input)
		name := strings.TrimSuffix(filepath.Base(path), ".request.json")
		var golden requestGolden
		readJSON(c, filepath.Join(testdata, "corpus", name+".golden.json"), &golden)
		if input.Kind == kind {
			cases = append(cases, requestCase{name: name, kind: input.Kind, request: input.Request, golden: golden})
		}
	}
	return cases
}

// The provider's facts, impacts and subject kinds in the words the host's
// golden files write them.
var (
	factNames = map[protocol.LintFact]string{
		protocol.LintFact_LINT_FACT_STATEFUL_DROP: "stateful-drop", protocol.LintFact_LINT_FACT_UNIQUE_INDEX: "unique-index",
		protocol.LintFact_LINT_FACT_REBUILD_DROP: "rebuild-drop", protocol.LintFact_LINT_FACT_REBUILD_RENAME: "rebuild-rename",
		protocol.LintFact_LINT_FACT_SAFETY_EFFECT: "safety-effect",
	}
	impactNames = map[protocol.LintImpact]string{
		protocol.LintImpact_LINT_IMPACT_BEHAVIORAL: "behavioral", protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE: "destructive",
	}
	kindNames = map[protocol.LintSubject_Kind]string{protocol.LintSubject_TABLE: "table", protocol.LintSubject_COLUMN: "column"}
)

func wireFact(name string) protocol.LintFact {
	for fact, known := range factNames {
		if known == name {
			return fact
		}
	}
	return protocol.LintFact_LINT_FACT_UNSPECIFIED
}

func wireImpact(name string) protocol.LintImpact {
	for impact, known := range impactNames {
		if known == name {
			return impact
		}
	}
	return protocol.LintImpact_LINT_IMPACT_UNSPECIFIED
}

func wireSources(sources []source) []*protocol.LintSource {
	result := make([]*protocol.LintSource, 0, len(sources))
	for _, s := range sources {
		result = append(result, &protocol.LintSource{Name: s.Name, Sql: s.SQL, Version: s.Version, Up: s.Up, Down: s.Down, Repeatable: s.Repeatable})
	}
	return result
}

func wireStatements(statements []statement) []*protocol.LintStatement {
	result := make([]*protocol.LintStatement, 0, len(statements))
	for _, s := range statements {
		wire := &protocol.LintStatement{Start: uint32(s.Start), End: uint32(s.End), Line: uint32(s.Line), Sql: s.SQL, Canonical: s.Canonical,
			Words: s.Words, SourceWords: s.SourceWords, Comments: s.Comments}
		for _, e := range s.Effects {
			wire.Effects = append(wire.Effects, &protocol.LintEffect{Fact: wireFact(e.Fact), Object: e.Object, Kind: e.Kind, Table: e.Table,
				Columns: e.Columns, Clause: e.Clause, Failure: e.Failure, Impact: wireImpact(e.Impact), Reason: e.Reason})
		}
		result = append(result, wire)
	}
	return result
}

func goldenEffects(effects []*protocol.LintEffect) []effect {
	var result []effect
	for _, e := range effects {
		result = append(result, effect{Fact: factNames[e.GetFact()], Object: e.GetObject(), Kind: e.GetKind(), Table: e.GetTable(),
			Columns: e.GetColumns(), Clause: e.GetClause(), Failure: e.GetFailure(), Impact: impactNames[e.GetImpact()], Reason: e.GetReason()})
	}
	return result
}

func goldenStatements(statements []*protocol.LintStatement) []statement {
	var result []statement
	for _, s := range statements {
		result = append(result, statement{Start: int(s.GetStart()), End: int(s.GetEnd()), Line: int(s.GetLine()), SQL: s.GetSql(),
			Canonical: s.GetCanonical(), Words: s.GetWords(), SourceWords: s.GetSourceWords(), Comments: s.GetComments(),
			Effects: goldenEffects(s.GetEffects())})
	}
	return result
}

// sameAnswer compares two answers with an empty list and a missing one alike:
// the golden files write either, and so does the wire.
var sameAnswer = qt.CmpEquals(cmpopts.EquateEmpty())

// pluginSplit is the provider's split of the sources of a recorded split.
func pluginSplit(c *qt.C, call splitCall) splitCall {
	c.Helper()
	result, err := migrationlint.SplitRequest(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: wireSources(call.Sources)}, false)
	c.Assert(err, qt.IsNil)
	answer := splitCall{Sources: call.Sources}
	for _, file := range result.GetFiles() {
		answer.Files = append(answer.Files, goldenStatements(file.GetStatements()))
	}
	for _, fact := range result.GetJudged() {
		answer.Judged = append(answer.Judged, factNames[fact])
	}
	return answer
}

// TestCorpus_SplitsAsTheHost requires every split the host's owner made to be
// the provider's split of the same sources.
func TestCorpus_SplitsAsTheHost(t *testing.T) {
	for _, row := range directoryCases(qt.New(t)) {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			for _, call := range row.golden.Splits {
				c.Assert(pluginSplit(c, call), sameAnswer, call)
			}
		})
	}
}

// targets are the capability maps the host resolves for each server version.
func targets(c *qt.C) map[string]map[string]bool {
	c.Helper()
	var recorded struct {
		Targets map[string]map[string]bool `json:"targets"`
	}
	readJSON(c, filepath.Join(testdata, "targets.golden.json"), &recorded)
	return recorded.Targets
}

// publishedCodes are the codes of the rules the provider publishes.
func publishedCodes() []string {
	var codes []string
	for _, rule := range migrationlint.Rules() {
		codes = append(codes, rule.GetCode())
	}
	return codes
}

// checkRequest replays a recorded check as the request the host sent, with
// the codes the provider publishes.
func checkRequest(c *qt.C, golden directoryGolden, r run, resolved map[string]map[string]bool) *protocol.CheckLintRequest {
	c.Helper()
	call := r.Check
	capabilities := maps.Clone(resolved[call.ServerVersion])
	maps.Copy(capabilities, call.Overrides)
	request := &protocol.CheckLintRequest{Engine: "ydb", ServerVersion: call.ServerVersion, Capabilities: capabilities,
		Compatibility: call.Compatibility}
	request.Codes = servedCodes(call.Codes)
	for _, file := range call.Files {
		statements := file.Statements
		if file.Source >= 0 {
			statements = slices.Clone(golden.Splits[r.Split].Files[file.Source])
			for i := range statements {
				statements[i].Line += file.LineOffset
			}
		}
		request.Files = append(request.Files, &protocol.LintFile{Name: file.Name, Version: file.Version, Up: file.Up, Down: file.Down,
			HasPair: file.HasPair, NoTransaction: file.NoTransaction, Repeatable: file.Repeatable, Statements: wireStatements(statements)})
	}
	return request
}

// pluginFindings are the provider's findings for request, in the order the
// host reports an owner's findings: by file, statement and code, each rule's
// own order kept.
func pluginFindings(c *qt.C, request *protocol.CheckLintRequest) []finding {
	c.Helper()
	result, err := migrationlint.CheckRequest(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetComplete(), qt.IsTrue)
	var findings []finding
	for _, f := range result.GetFindings() {
		value := finding{Code: f.GetCode(), File: int(f.GetFile()), Statement: int(f.GetStatement()), Message: f.GetMessage()}
		for _, s := range f.GetSubjects() {
			value.Subjects = append(value.Subjects, subject{Kind: kindNames[s.GetKind()], Name: s.GetName(), Parent: s.GetParent(), DataType: s.GetDataType()})
		}
		findings = append(findings, value)
	}
	slices.SortStableFunc(findings, func(a, b finding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Statement, b.Statement), cmp.Compare(a.Code, b.Code))
	})
	return findings
}

// servedCodes are the codes of codes the provider publishes, in order.
func servedCodes(codes []string) []string {
	return slices.DeleteFunc(slices.Clone(codes), func(code string) bool { return !slices.Contains(publishedCodes(), code) })
}

// servedFindings are the findings of the codes the provider publishes.
func servedFindings(findings []finding) []finding {
	return slices.DeleteFunc(slices.Clone(findings), func(f finding) bool { return !slices.Contains(publishedCodes(), f.Code) })
}

// checkedRuns are the runs of a case that sent the owner a check.
func checkedRuns(golden directoryGolden) []run {
	return slices.DeleteFunc(slices.Clone(golden.Runs), func(r run) bool { return r.Check == nil })
}

// TestCorpus_ChecksAsTheHost requires every check the host's owner answered
// to be answered by the provider with the same findings.
func TestCorpus_ChecksAsTheHost(t *testing.T) {
	c := qt.New(t)
	resolved := targets(c)
	for _, row := range directoryCases(c) {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			for _, r := range checkedRuns(row.golden) {
				request := checkRequest(c, row.golden, r, resolved)
				c.Assert(pluginFindings(c, request), sameAnswer, servedFindings(r.Check.Findings),
					qt.Commentf("server version %q, compatibility %q", r.ServerVersion, r.Compatibility))
			}
		})
	}
}

// annotatedRuns are the runs of a case that asked the owners for annotations.
func annotatedRuns(golden directoryGolden) []run {
	return slices.DeleteFunc(slices.Clone(golden.Runs), func(r run) bool { return r.Annotate == nil })
}

// annotateRequest replays a recorded annotation as the request the host sent.
func annotateRequest(golden directoryGolden, call *annotateCall) *protocol.AnnotateLintRequest {
	sources := call.Sources
	if call.SourcesOf >= 0 {
		sources = golden.Splits[call.SourcesOf].Sources
	}
	request := &protocol.AnnotateLintRequest{Engine: "ydb", Sources: wireSources(sources)}
	for _, fact := range call.Facts {
		request.Facts = append(request.Facts, wireFact(fact))
	}
	for i, statements := range call.Files {
		file := &protocol.LintFile{Name: "unpaired", Statements: wireStatements(statements)}
		if i < len(sources) {
			file.Name = sources[i].Name
		}
		request.Files = append(request.Files, file)
	}
	return request
}

// pluginAnnotations are the provider's answers to request.
func pluginAnnotations(c *qt.C, request *protocol.AnnotateLintRequest) [][]annotation {
	c.Helper()
	result, err := migrationlint.AnnotateRequest(c.Context(), request)
	c.Assert(err, qt.IsNil)
	answers := make([][]annotation, 0, len(result.GetFiles()))
	for _, file := range result.GetFiles() {
		var answer []annotation
		for _, a := range file.GetStatements() {
			answer = append(answer, annotation{Effects: goldenEffects(a.GetEffects()), Declined: a.GetDeclined()})
		}
		answers = append(answers, answer)
	}
	return answers
}

// TestCorpus_AnnotatesAsTheHost requires every annotation of a run that names
// no dialect to be the provider's annotation of the same statements.
func TestCorpus_AnnotatesAsTheHost(t *testing.T) {
	for _, row := range directoryCases(qt.New(t)) {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			for _, r := range annotatedRuns(row.golden) {
				c.Assert(pluginAnnotations(c, annotateRequest(row.golden, r.Annotate)), sameAnswer, r.Annotate.Answer)
			}
		})
	}
}
