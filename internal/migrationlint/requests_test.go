package migrationlint_test

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// The request cases are requests the host's own tests send its YDB owner
// directly, with edge cases a migration directory does not reach, and the
// owner's answers to them.

type splitInput struct {
	Sources []source `json:"sources"`
}

type annotateInput struct {
	Facts   []string      `json:"facts"`
	Sources []source      `json:"sources"`
	Files   [][]statement `json:"files"`
}

type checkRequestFile struct {
	Name          string      `json:"name"`
	Version       int64       `json:"version"`
	Up            bool        `json:"up"`
	Down          bool        `json:"down"`
	HasPair       bool        `json:"has_pair"`
	NoTransaction bool        `json:"no_transaction"`
	Repeatable    bool        `json:"repeatable"`
	Statements    []statement `json:"statements"`
}

type checkInput struct {
	Target struct {
		Version      string          `json:"version"`
		Capabilities map[string]bool `json:"capabilities"`
	} `json:"target"`
	Codes         []string           `json:"codes"`
	Files         []checkRequestFile `json:"files"`
	Compatibility string             `json:"compatibility"`
}

func decode[T any](c *qt.C, data json.RawMessage) T {
	c.Helper()
	var value T
	c.Assert(json.Unmarshal(data, &value), qt.IsNil)
	return value
}

// TestRequests_SplitAsTheHost requires the provider's split of every split
// request the host's owner answered to be the owner's.
func TestRequests_SplitAsTheHost(t *testing.T) {
	cases := requestCases(qt.New(t), "split")
	qt.New(t).Assert(len(cases) >= 6, qt.IsTrue)
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := decode[splitInput](c, row.request)
			want := decode[splitCall](c, row.golden.Result)
			want.Sources = request.Sources
			c.Assert(row.golden.Error, qt.Equals, "")
			c.Assert(pluginSplit(c, splitCall{Sources: request.Sources}), sameAnswer, want)
		})
	}
}

// annotationsOf is the answer an annotate request case recorded.
type annotationsOf struct {
	Files [][]annotation `json:"files"`
}

// answeredAnnotations are the annotate request cases the host's owner
// answered, and refusedAnnotations those it refused.
func answeredAnnotations(c *qt.C) []requestCase {
	c.Helper()
	return slices.DeleteFunc(requestCases(c, "annotate"), func(row requestCase) bool { return row.golden.Error != "" })
}

func refusedAnnotations(c *qt.C) []requestCase {
	c.Helper()
	return slices.DeleteFunc(requestCases(c, "annotate"), func(row requestCase) bool { return row.golden.Error == "" })
}

// wireAnnotateRequest is an annotate request case as the host sends it.
func wireAnnotateRequest(input annotateInput) *protocol.AnnotateLintRequest {
	return annotateRequest(directoryGolden{}, &annotateCall{Facts: input.Facts, SourcesOf: -1, Sources: input.Sources, Files: input.Files})
}

// TestRequests_AnnotateAsTheHost requires the provider's answer to every
// annotate request the host's owner answered to be the owner's.
func TestRequests_AnnotateAsTheHost(t *testing.T) {
	cases := answeredAnnotations(qt.New(t))
	qt.New(t).Assert(len(cases) >= 12, qt.IsTrue)
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := decode[annotateInput](c, row.request)
			want := decode[annotationsOf](c, row.golden.Result)
			c.Assert(pluginAnnotations(c, wireAnnotateRequest(request)), sameAnswer, want.Files)
		})
	}
}

// TestRequests_AnnotateRefusesWhatTheHostRefuses requires every annotate
// request the host's owner refused to be refused before it reaches the
// provider: the SDK validates a request against the provider's description.
func TestRequests_AnnotateRefusesWhatTheHostRefuses(t *testing.T) {
	cases := refusedAnnotations(qt.New(t))
	qt.New(t).Assert(cases, qt.Not(qt.HasLen), 0)
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := decode[annotateInput](c, row.request)
			err := provider.ValidateAnnotateLintRequest(backend.Description(), wireAnnotateRequest(request))
			c.Assert(err, qt.ErrorIs, provider.ErrProtocol, qt.Commentf("the host refuses it with: %s", row.golden.Error))
		})
	}
}

// TestRequests_CheckAsTheHost requires the provider's findings for every
// check request the host's owner answered to be the owner's.
func TestRequests_CheckAsTheHost(t *testing.T) {
	cases := requestCases(qt.New(t), "check")
	qt.New(t).Assert(len(cases) >= 5, qt.IsTrue)
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := decode[checkInput](c, row.request)
			want := decode[struct {
				Complete bool      `json:"complete"`
				Findings []finding `json:"findings"`
			}](c, row.golden.Result)
			wire := &protocol.CheckLintRequest{Engine: "ydb", ServerVersion: request.Target.Version, Capabilities: maps.Clone(request.Target.Capabilities),
				Codes: servedCodes(request.Codes), Compatibility: request.Compatibility}
			for _, file := range request.Files {
				wire.Files = append(wire.Files, &protocol.LintFile{Name: file.Name, Version: file.Version, Up: file.Up, Down: file.Down, HasPair: file.HasPair,
					NoTransaction: file.NoTransaction, Repeatable: file.Repeatable, Statements: wireStatements(file.Statements)})
			}
			c.Assert(want.Complete, qt.IsTrue)
			c.Assert(pluginFindings(c, wire), sameAnswer, servedFindings(want.Findings))
		})
	}
}

// publishedRule is a rule as rules.golden.json records it.
type publishedRule struct {
	Code          string   `json:"code"`
	Title         string   `json:"title"`
	Severity      string   `json:"severity"`
	AppliesToDown bool     `json:"applies_to_down"`
	Subsumes      []string `json:"subsumes"`
	Input         string   `json:"input"`
	Targets       []string `json:"targets"`
}

var severityNames = map[protocol.LintRule_Severity]string{
	protocol.LintRule_INFO: "info", protocol.LintRule_WARNING: "warning", protocol.LintRule_ERROR: "error",
}

// pendingCodes are the rules of the host's YDB owner the provider does not
// serve yet: the rules that read a statement through a reader of their own,
// and the rules that read the directory's history.
var pendingCodes = []string{
	"YD102", "YD104", "YD105", "YD106", "YD107", "YD108", "YD109", "YD115", "YD118", "YD119", "YD131", "YD140", "YD141", "YD150", "YD160",
}

// TestRules_AreTheHosts requires the provider to publish the rules the host's
// owner publishes, in code order, each with its title, severity, reach into
// down migrations and the rules it subsumes, apart from the pending ones.
// Every one reads the statement text and judges YDB alone, which the wire
// takes as given.
func TestRules_AreTheHosts(t *testing.T) {
	c := qt.New(t)
	var recorded struct {
		Rules []publishedRule `json:"rules"`
	}
	readJSON(c, filepath.Join(testdata, "rules.golden.json"), &recorded)
	var got []publishedRule
	for _, rule := range migrationlint.Rules() {
		got = append(got, publishedRule{Code: rule.GetCode(), Title: rule.GetTitle(), Severity: severityNames[rule.GetSeverity()],
			AppliesToDown: rule.GetAppliesToDown(), Subsumes: rule.GetSubsumes(), Targets: []string{"ydb"}})
	}
	c.Assert(got, sameAnswer, slices.DeleteFunc(recorded.Rules, func(rule publishedRule) bool { return slices.Contains(pendingCodes, rule.Code) }))
	var codes []string
	for _, rule := range recorded.Rules {
		codes = append(codes, rule.Code)
	}
	c.Assert(codes, qt.HasLen, len(got)+len(pendingCodes))
}
