package migrationlint_test

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
)

// corpusHosts are the host commits the golden files name.
func corpusHosts(c *qt.C) []string {
	c.Helper()
	hosts := map[string]bool{}
	for _, row := range directoryCases(c) {
		hosts[row.golden.Host] = true
	}
	for _, kind := range []string{"split", "annotate", "check"} {
		for _, row := range requestCases(c, kind) {
			hosts[row.golden.Host] = true
		}
	}
	for _, name := range []string{"rules.golden.json", "targets.golden.json"} {
		var recorded struct {
			Host string `json:"host"`
		}
		readJSON(c, filepath.Join(testdata, name), &recorded)
		hosts[recorded.Host] = true
	}
	return slices.Sorted(maps.Keys(hosts))
}

// corpusFindings counts the findings of each code the corpus records.
func corpusFindings(c *qt.C) map[string]int {
	c.Helper()
	counts := map[string]int{}
	for _, row := range directoryCases(c) {
		for _, r := range checkedRuns(row.golden) {
			for _, f := range r.Check.Findings {
				counts[f.Code]++
			}
		}
	}
	return counts
}

// corpusFacts counts the effects of each fact the corpus's splits record.
func corpusFacts(c *qt.C) map[string]int {
	c.Helper()
	counts := map[string]int{}
	for _, row := range directoryCases(c) {
		for _, call := range row.golden.Splits {
			for _, file := range call.Files {
				for _, s := range file {
					for _, e := range s.Effects {
						counts[e.Fact]++
					}
				}
			}
		}
	}
	return counts
}

// corpusDeclines counts the statements the corpus records an annotation
// declining.
func corpusDeclines(c *qt.C) int {
	c.Helper()
	declined := 0
	for _, row := range directoryCases(c) {
		for _, r := range annotatedRuns(row.golden) {
			for _, file := range r.Annotate.Answer {
				declined += len(slices.DeleteFunc(slices.Clone(file), func(a annotation) bool { return !a.Declined }))
			}
		}
	}
	for _, row := range answeredAnnotations(c) {
		annotations := decode[annotationsOf](c, row.golden.Result)
		for _, file := range annotations.Files {
			declined += len(slices.DeleteFunc(slices.Clone(file), func(a annotation) bool { return !a.Declined }))
		}
	}
	return declined
}

// The corpus is the measurement, so it must not shrink without notice, every
// golden file must come from one host commit, and every rule and every fact
// the provider reports must be answered by the host somewhere in it: a case
// list that stopped matching would otherwise compare nothing and pass.
func TestCorpus_Shape(t *testing.T) {
	c := qt.New(t)
	c.Assert(len(directoryCases(c)) >= 220, qt.IsTrue, qt.Commentf("the corpus holds %d directories", len(directoryCases(c))))
	c.Assert(len(requestCases(c, "split"))+len(requestCases(c, "annotate"))+len(requestCases(c, "check")) >= 25, qt.IsTrue)
	hosts := corpusHosts(c)
	c.Assert(hosts, qt.HasLen, 1)
	c.Assert(hosts[0], qt.Matches, `[0-9a-f]{40}`)
	findings := corpusFindings(c)
	for _, code := range publishedCodes() {
		c.Assert(findings[code] > 0, qt.IsTrue, qt.Commentf("no case of the corpus reports %s", code))
	}
	facts := corpusFacts(c)
	for _, fact := range migrationlint.Judged() {
		c.Assert(facts[factNames[fact]] > 0, qt.IsTrue, qt.Commentf("no split of the corpus reports %s", fact))
	}
	c.Assert(corpusDeclines(c) > 0, qt.IsTrue)
}
