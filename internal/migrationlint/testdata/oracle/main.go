// Command ydblintoracle records what the Ptah host's bundled YDB lint owner
// (dialect/ydb/ydblint, with internal/lintscan and the readers it uses)
// answers for each input of the corpus, so the provider's migration lint can
// be compared with it finding for finding without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages behind an internal boundary, so it is copied into a checkout
// of the host and run from there:
//
//	mkdir -p <host>/cmd/ydblintoracle
//	cp internal/migrationlint/testdata/oracle/main.go <host>/cmd/ydblintoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydblintoracle <plugin>/internal/migrationlint/testdata)
//
// It reads <dir>/corpus and writes three kinds of golden file:
//
//   - <dir>/rules.golden.json holds the rules the YDB owner publishes.
//   - <dir>/targets.golden.json holds, for each server version a run names,
//     the capability map the host resolves for it, which every check request
//     of that version carries.
//   - <dir>/corpus/<case>.golden.json holds the host's answer for one case.
//
// A case is a migration directory, <dir>/corpus/<case>/, or a request the
// host sends the owner, <dir>/corpus/<case>.request.json. A directory is
// linted through migration/lint, the code `ptah migrations lint` runs, once
// per run: with dialect ydb on the newest line, on 25.1.4.7 and on the
// Atlas-compatible surface, with no dialect, and with every run that
// <dir>/corpus/<case>.runs.json lists. A recording runtime wraps the bundled
// one and keeps every split, annotation and check request the run sends to
// the owners, with the owners' answers, so the golden file holds exactly what
// crosses the boundary a provider serves. A request case is sent to the
// bundled runtime as it is.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"ptah.run/core/lintext"
	"ptah.run/core/platform/capability"
	"ptah.run/engine"
	"ptah.run/engine/builtin"
	"ptah.run/migration/lint"
	"ptah.run/migration/migrationfile"
)

// runSpec is one lint run of a directory case.
type runSpec struct {
	Dialect       string          `json:"dialect"`
	ServerVersion string          `json:"server_version"`
	Compatibility string          `json:"compatibility"`
	DirFormat     string          `json:"dir_format,omitempty"`
	Disabled      []string        `json:"disabled,omitempty"`
	Overrides     map[string]bool `json:"overrides,omitempty"`
}

// standardRuns are the runs every directory case gets.
var standardRuns = []runSpec{
	{Dialect: "ydb"},
	{Dialect: "ydb", ServerVersion: "25.1.4.7"},
	{Dialect: "ydb", Compatibility: "atlas"},
	{},
}

type splitCall struct {
	Sources []lintext.Source      `json:"sources"`
	Files   [][]lintext.Statement `json:"files"`
	Judged  []lintext.Fact        `json:"judged"`
}

// annotateCall is an annotation request and its answer. Its sources are those
// of the split SourcesOf names, and are written out only where no split of the
// case holds them.
type annotateCall struct {
	Facts     []lintext.Fact         `json:"facts"`
	SourcesOf int                    `json:"sources_of"`
	Sources   []lintext.Source       `json:"sources,omitempty"`
	Files     [][]lintext.Statement  `json:"files"`
	Answer    [][]lintext.Annotation `json:"answer"`
	Declined  []lintext.Declined     `json:"declined,omitempty"`
}

// checkFile is a file of a check request. Its statements are those of the
// split source of the same name with each line moved by LineOffset, and are
// written out only where they are not.
type checkFile struct {
	Name          string              `json:"name"`
	Version       int64               `json:"version,omitempty"`
	Up            bool                `json:"up,omitempty"`
	Down          bool                `json:"down,omitempty"`
	HasPair       bool                `json:"has_pair,omitempty"`
	NoTransaction bool                `json:"no_transaction,omitempty"`
	Repeatable    bool                `json:"repeatable,omitempty"`
	Source        int                 `json:"source"`
	LineOffset    int                 `json:"line_offset,omitempty"`
	Statements    []lintext.Statement `json:"statements,omitempty"`
}

type checkCall struct {
	ServerVersion string            `json:"server_version"`
	Overrides     map[string]bool   `json:"overrides,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Codes         []string          `json:"codes"`
	Files         []checkFile       `json:"files"`
	Findings      []lintext.Finding `json:"findings"`
}

type run struct {
	runSpec
	Error    string        `json:"error,omitempty"`
	Split    int           `json:"split"`
	Annotate *annotateCall `json:"annotate,omitempty"`
	Check    *checkCall    `json:"check,omitempty"`
	// Report is what the run reported, file:line:rule, for a reader.
	Report []string `json:"report"`
}

type directoryGolden struct {
	Host   string      `json:"host"`
	Splits []splitCall `json:"splits"`
	Runs   []run       `json:"runs"`
}

type requestCase struct {
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request"`
}

type requestGolden struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

type publishedRule struct {
	Code          string           `json:"code"`
	Title         string           `json:"title"`
	Severity      lintext.Severity `json:"severity"`
	AppliesToDown bool             `json:"applies_to_down"`
	Subsumes      []string         `json:"subsumes,omitempty"`
	Input         lintext.Input    `json:"input,omitempty"`
	Targets       []string         `json:"targets"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydblintoracle <testdata directory>")
		os.Exit(2)
	}
	host := os.Getenv("HOST_COMMIT")
	if host == "" {
		fail(errors.New("HOST_COMMIT names the host commit the goldens are recorded at"))
	}
	base, err := builtin.Bundled()
	check(err)
	root := os.Args[1]
	corpus := filepath.Join(root, "corpus")
	rules, err := base.LintRules("ydb")
	check(err)
	published := make([]publishedRule, 0, len(rules))
	for _, rule := range rules {
		published = append(published, publishedRule{Code: rule.Code, Title: rule.Title, Severity: rule.Severity, AppliesToDown: rule.AppliesToDown,
			Subsumes: rule.Subsumes, Input: rule.Input, Targets: rule.Targets})
	}
	write(filepath.Join(root, "rules.golden.json"), map[string]any{"host": host, "rules": published})

	entries, err := os.ReadDir(corpus)
	check(err)
	targets := map[string]map[string]bool{}
	cases := 0
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir():
			runs := slices.Clone(standardRuns)
			if data, err := os.ReadFile(filepath.Join(corpus, name+".runs.json")); err == nil {
				var extra []runSpec
				check(json.Unmarshal(data, &extra))
				for _, spec := range extra {
					if !slices.ContainsFunc(runs, func(known runSpec) bool { return reflect.DeepEqual(known, spec) }) {
						runs = append(runs, spec)
					}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				fail(err)
			}
			golden := directoryCase(base, filepath.Join(corpus, name), runs, targets)
			golden.Host = host
			write(filepath.Join(corpus, name+".golden.json"), golden)
			cases++
		case strings.HasSuffix(name, ".request.json"):
			golden := requestCaseOf(base, filepath.Join(corpus, name))
			golden.Host = host
			write(filepath.Join(corpus, strings.TrimSuffix(name, ".request.json")+".golden.json"), golden)
			cases++
		}
	}
	if cases == 0 {
		fail(fmt.Errorf("no case under %s", corpus))
	}
	write(filepath.Join(root, "targets.golden.json"), map[string]any{"host": host, "targets": targets})
}

// directoryCase lints one directory once per run.
func directoryCase(base *engine.Runtime, dir string, runs []runSpec, targets map[string]map[string]bool) directoryGolden {
	var golden directoryGolden
	for _, spec := range runs {
		recorded := run{runSpec: spec, Split: -1}
		opts := lint.Options{Dialect: spec.Dialect, Compatibility: lint.CompatibilityProfile(spec.Compatibility), Disabled: spec.Disabled,
			DirFormat: migrationfile.DirFormat(spec.DirFormat)}
		if spec.Dialect != "" {
			target, err := lint.ResolveTarget(spec.Dialect, spec.ServerVersion)
			check(err)
			targets[spec.ServerVersion] = capabilityMap(target.Capabilities)
			for _, key := range slices.Sorted(maps.Keys(spec.Overrides)) {
				target.Capabilities = target.Capabilities.With(capability.Capability(key), spec.Overrides[key])
			}
			opts.Target = target
		}
		recorder := &recording{Runtime: base}
		analysis, err := lint.AnalyzeFS(context.Background(), recorder, os.DirFS(dir), opts)
		if err != nil {
			recorded.Error = err.Error()
		} else {
			recorded.Report = []string{}
			for _, finding := range analysis.Findings() {
				recorded.Report = append(recorded.Report, fmt.Sprintf("%s:%d:%s", finding.File, finding.Line, finding.Rule))
			}
		}
		if len(recorder.splits) > 1 || len(recorder.annotations) > 1 || len(recorder.checks) > 1 {
			fail(fmt.Errorf("%s: a run made more than one call of a kind", dir))
		}
		var sources []lintext.Source
		if len(recorder.splits) == 1 {
			call := recorder.splits[0]
			sources = call.Sources
			recorded.Split = slices.IndexFunc(golden.Splits, func(known splitCall) bool { return reflect.DeepEqual(known, call) })
			if recorded.Split < 0 {
				recorded.Split = len(golden.Splits)
				golden.Splits = append(golden.Splits, call)
			}
		}
		if len(recorder.annotations) == 1 {
			call := recorder.annotations[0]
			call.SourcesOf = slices.IndexFunc(golden.Splits, func(known splitCall) bool { return reflect.DeepEqual(known.Sources, call.Sources) })
			if call.SourcesOf >= 0 {
				call.Sources = nil
			}
			recorded.Annotate = &call
		}
		if len(recorder.checks) == 1 {
			request, result := recorder.checks[0].request, recorder.checks[0].result
			switch {
			case spec.Dialect == "":
				// A run that names no target sends every rule, YD included, and
				// the YD rules say nothing about text the YQL lexer did not read.
				for _, finding := range result.Findings {
					if strings.HasPrefix(finding.Code, "YD") {
						fail(fmt.Errorf("%s: %s reported on a run that names no dialect", dir, finding.Code))
					}
				}
			default:
				recorded.Check = checkOf(request, result, sources, golden.Splits[recorded.Split].Files, targets[spec.ServerVersion])
			}
		}
		golden.Runs = append(golden.Runs, recorded)
	}
	return golden
}

// checkOf is a check request and its answer, with each file's statements
// referred to the split they came from where they match it.
func checkOf(request lintext.CheckRequest, result lintext.CheckResult, sources []lintext.Source, split [][]lintext.Statement,
	resolved map[string]bool,
) *checkCall {
	carried := capabilityMap(request.Target.Capabilities)
	call := &checkCall{ServerVersion: request.Target.Version, Compatibility: request.Compatibility, Codes: request.Codes,
		Findings: result.Findings}
	if call.Findings == nil {
		call.Findings = []lintext.Finding{}
	}
	for _, key := range slices.Sorted(maps.Keys(carried)) {
		if value, known := resolved[key]; !known || value != carried[key] {
			if call.Overrides == nil {
				call.Overrides = map[string]bool{}
			}
			call.Overrides[key] = carried[key]
		}
	}
	for key := range resolved {
		if _, known := carried[key]; !known {
			fail(fmt.Errorf("a check request leaves out capability %s", key))
		}
	}
	for _, file := range request.Files {
		entry := checkFile{Name: file.Name, Version: file.Version, Up: file.Up, Down: file.Down, HasPair: file.HasPair,
			NoTransaction: file.NoTransaction, Repeatable: file.Repeatable, Source: -1}
		source := slices.IndexFunc(sources, func(source lintext.Source) bool { return source.Name == file.Name })
		if source >= 0 && matches(file.Statements, split[source]) {
			entry.Source = source
			if len(file.Statements) > 0 {
				entry.LineOffset = file.Statements[0].Line - split[source][0].Line
			}
		} else {
			entry.Statements = file.Statements
		}
		call.Files = append(call.Files, entry)
	}
	return call
}

// matches reports whether statements are split's moved by one line offset.
func matches(statements, split []lintext.Statement) bool {
	if len(statements) != len(split) {
		return false
	}
	if len(statements) == 0 {
		return true
	}
	offset := statements[0].Line - split[0].Line
	for i := range statements {
		moved := split[i]
		moved.Line += offset
		if !reflect.DeepEqual(statements[i], moved) {
			return false
		}
	}
	return true
}

// requestCaseOf sends one recorded request to the bundled runtime.
func requestCaseOf(base *engine.Runtime, path string) requestGolden {
	data, err := os.ReadFile(path)
	check(err)
	var input requestCase
	check(json.Unmarshal(data, &input))
	golden := requestGolden{Kind: input.Kind}
	ctx := context.Background()
	var result any
	switch input.Kind {
	case "split":
		var request lintext.SplitRequest
		check(json.Unmarshal(input.Request, &request))
		result, err = base.SplitStatements(ctx, request)
	case "annotate":
		var request lintext.AnnotateRequest
		check(json.Unmarshal(input.Request, &request))
		result, err = base.AnnotateStatements(ctx, request)
	case "check":
		var request lintext.CheckRequest
		check(json.Unmarshal(input.Request, &request))
		if request.Target.Name != "ydb" {
			fail(fmt.Errorf("%s: a provider is sent check requests for its own engine only", path))
		}
		result, err = base.CheckMigrations(ctx, request)
	default:
		fail(fmt.Errorf("%s: unknown kind %q", path, input.Kind))
	}
	if err != nil {
		golden.Error = err.Error()
		return golden
	}
	golden.Result = result
	return golden
}

// recording is the bundled runtime, keeping every request it is sent.
type recording struct {
	*engine.Runtime
	splits      []splitCall
	annotations []annotateCall
	checks      []struct {
		request lintext.CheckRequest
		result  lintext.CheckResult
	}
}

func (r *recording) SplitStatements(ctx context.Context, request lintext.SplitRequest) (lintext.SplitResult, error) {
	result, err := r.Runtime.SplitStatements(ctx, request)
	check(err)
	r.splits = append(r.splits, splitCall{Sources: request.Sources, Files: result.Files, Judged: result.Judged})
	return result, nil
}

func (r *recording) AnnotateStatements(ctx context.Context, request lintext.AnnotateRequest) (lintext.AnnotateResult, error) {
	result, err := r.Runtime.AnnotateStatements(ctx, request)
	check(err)
	r.annotations = append(r.annotations, annotateCall{Facts: request.Facts, Sources: request.Sources, Files: request.Files,
		Answer: result.Files, Declined: result.Declined})
	return result, nil
}

func (r *recording) CheckMigrations(ctx context.Context, request lintext.CheckRequest) (lintext.CheckResult, error) {
	result, err := r.Runtime.CheckMigrations(ctx, request)
	check(err)
	r.checks = append(r.checks, struct {
		request lintext.CheckRequest
		result  lintext.CheckResult
	}{request, result})
	return result, nil
}

func capabilityMap(capabilities capability.Capabilities) map[string]bool {
	result := map[string]bool{}
	for key, value := range capabilities.All() {
		result[string(key)] = value
	}
	return result
}

// write writes value as JSON, one key per line and an array of scalars on
// one line, so a regenerated golden file reads as a diff.
func write(path string, value any) {
	encoded, err := json.Marshal(value)
	check(err)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	check(decoder.Decode(&generic))
	var b strings.Builder
	pretty(&b, generic, "")
	b.WriteByte('\n')
	check(os.WriteFile(path, []byte(b.String()), 0o644))
}

func pretty(b *strings.Builder, value any, indent string) {
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		keys := slices.Sorted(maps.Keys(value))
		for i, key := range keys {
			b.WriteString(indent + " ")
			scalar(b, key)
			b.WriteString(": ")
			pretty(b, value[key], indent+" ")
			if i < len(keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if !slices.ContainsFunc(value, func(item any) bool {
			_, isMap := item.(map[string]any)
			_, isSlice := item.([]any)
			return isMap || isSlice
		}) {
			b.WriteByte('[')
			for i, item := range value {
				if i > 0 {
					b.WriteString(", ")
				}
				scalar(b, item)
			}
			b.WriteByte(']')
			return
		}
		b.WriteString("[\n")
		for i, item := range value {
			b.WriteString(indent + " ")
			pretty(b, item, indent+" ")
			if i < len(value)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	default:
		scalar(b, value)
	}
}

func scalar(b *strings.Builder, value any) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	check(encoder.Encode(value))
	b.WriteString(strings.TrimSuffix(buffer.String(), "\n"))
}

func check(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
