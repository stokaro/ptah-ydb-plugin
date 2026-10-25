//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// statementFactsCorpus is the provider's corpus of YQL files, with what the
// host's built-in YQL reading of `ptah sql lint` reports over each.
const statementFactsCorpus = "../internal/migrationlint/testdata/statementfacts"

type builtinLint struct {
	Versions map[string]struct {
		Findings []struct {
			Rule    string `json:"rule"`
			Line    int    `json:"line"`
			Column  int    `json:"column"`
			Message string `json:"message"`
		} `json:"findings"`
	} `json:"versions"`
}

// hostSQLLint runs the pinned host's `ptah sql lint` over one file with this
// provider selected for ydb on a server version, and returns each finding as
// line:column rule message, and how the host exited.
func hostSQLLint(c *qt.C, file, version string) ([]string, int) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "sql", "lint", "--provider-config", manifest, "--dialect", "ydb", "--server-version", version, "--format", "json", file)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	c.Assert(code == 0 || code == 1, qt.IsTrue, qt.Commentf("%v: %s%s", err, stdout.String(), stderr.String()))
	var report struct {
		Findings []struct {
			Rule    string `json:"rule"`
			Line    int    `json:"line"`
			Column  int    `json:"column"`
			Message string `json:"message"`
		} `json:"findings"`
	}
	c.Assert(json.Unmarshal(stdout.Bytes(), &report), qt.IsNil, qt.Commentf("stdout %q, stderr %q", stdout.String(), stderr.String()))
	findings := []string{}
	for _, finding := range report.Findings {
		findings = append(findings, fmt.Sprintf("%d:%d %s %s", finding.Line, finding.Column, finding.Rule, finding.Message))
	}
	return findings, code
}

var (
	missingKeyMessage  = regexp.MustCompile(`^(\d+:\d+ DDL001) table "(.*)" has no primary key.*$`)
	requirementMessage = regexp.MustCompile(`^(\d+:\d+ CAP001) .* requires target capability (\S+), unavailable on this target$`)
)

// about reduces a finding to what both readings say alike: the position and
// rule, and the table of a missing primary key or the key of a capability the
// target lacks. The two readings word those findings differently by design.
func about(findings []string) []string {
	result := []string{}
	for _, finding := range findings {
		finding = missingKeyMessage.ReplaceAllString(finding, "$1 $2")
		result = append(result, requirementMessage.ReplaceAllString(finding, "$1 $2"))
	}
	return result
}

// The host lints a YDB file through this provider by the statement facts of
// its split, and reports over every file of the corpus, on every release line
// it names, what its built-in YQL reading reports: the same rules at the same
// positions, about the same tables and capabilities.
func TestHostLintsYQLByTheStatementFacts_HappyPath(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(statementFactsCorpus, "*.yql"))
	qt.New(t).Assert(err, qt.IsNil)
	slices.Sort(files)
	qt.New(t).Assert(len(files) >= 12, qt.IsTrue)
	for _, file := range files {
		data, err := os.ReadFile(strings.TrimSuffix(file, ".yql") + ".golden.json")
		qt.New(t).Assert(err, qt.IsNil)
		var golden builtinLint
		qt.New(t).Assert(json.Unmarshal(data, &golden), qt.IsNil)
		for version, line := range golden.Versions {
			t.Run(filepath.Base(file)+"/"+version, func(t *testing.T) {
				c := qt.New(t)
				want := []string{}
				for _, finding := range line.Findings {
					want = append(want, fmt.Sprintf("%d:%d %s %s", finding.Line, finding.Column, finding.Rule, finding.Message))
				}

				got, _ := hostSQLLint(c, file, version)

				c.Assert(about(got), qt.DeepEquals, about(want))
			})
		}
	}
}

// The host words what the provider's facts name: the table, and the part of
// the statement a capability belongs to. A statement that starts with no word
// is one the linter does not lint, and the first statement of a file that
// opens with a byte order mark is linted, where the built-in reading skips
// both.
func TestHostWordsTheStatementFacts_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, file, version string
		code                int
		want                []string
	}{
		{name: "vector indexes on 25.1", file: "vectors.yql", version: "25.1.4.7", code: 1, want: []string{
			"1:1 CAP001 INDEX d_e of table d (USING vector_kmeans_tree) requires target capability vector_indexes, unavailable on this target",
			"3:1 CAP001 INDEX d_f of table d (ADD INDEX) requires target capability vector_indexes, unavailable on this target",
			"5:1 CAP001 INDEX d_b of table d (ADD INDEX) requires target capability vector_indexes, unavailable on this target",
			"5:1 CAP001 INDEX d_b of table d (ADD INDEX) requires target capability vector_bit_type, unavailable on this target",
			`7:1 DDL001 table "bits" has no primary key`,
			"7:1 CAP001 INDEX bits_e of table bits (USING vector_kmeans_tree) requires target capability vector_indexes, unavailable on this target",
			"7:1 CAP001 INDEX bits_e of table bits (USING vector_kmeans_tree) requires target capability vector_bit_type, unavailable on this target",
		}},
		{name: "a table named by a named expression on 25.1", file: "stricter/named-table.yql", version: "25.1.4.7", code: 1, want: []string{
			"1:1 SQL002 ptah sql lint does not lint named expression statements yet",
			`2:1 DDL001 table "$t" has no primary key`,
			"3:1 CAP001 COLUMN v of table $t (ADD COLUMN) requires target capability add_column_with_default, unavailable on this target",
		}},
		{name: "expressions", file: "stricter/expressions.yql", version: "26.2.1.14", code: 1, want: []string{
			"1:1 SQL002 ptah sql lint does not lint expression statements yet",
			"2:1 SQL002 ptah sql lint does not lint 1 statements yet",
			"3:1 SQL002 ptah sql lint does not lint expression statements yet",
		}},
		{name: "a byte order mark", file: "stricter/bom.yql", version: "26.2.1.14", code: 1, want: []string{
			`1:2 DDL001 table "bom" has no primary key`,
			"2:1 SQL002 ptah sql lint does not lint UPSERT statements yet",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			got, code := hostSQLLint(c, filepath.Join(statementFactsCorpus, test.file), test.version)

			c.Assert(got, qt.DeepEquals, test.want)
			c.Assert(code, qt.Equals, test.code)
		})
	}
}
