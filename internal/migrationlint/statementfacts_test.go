package migrationlint_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
)

// testdata/statementfacts holds YQL files and what the Ptah host's built-in
// YQL reading of `ptah sql lint` reports over each on three YDB release lines,
// recorded by testdata/statementfacts/oracle at one host commit with the
// capabilities the host resolves for each line. A host that lints a YDB file
// through this provider reads the statement facts of its split instead
// (internal/sqllint, lintOwned). ownedFindings is that reading, and the tests
// below hold it to the built-in one, finding for finding.
//
// The two readings word the primary key and the capability findings
// differently by design, so those compare by the table and the capability
// they name. The statements that start with no word are the one deliberate
// difference, which TestStatementFactsReportAnExpressionStatement pins.

type hostFinding struct {
	Rule    string `json:"rule"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

type hostLine struct {
	Capabilities map[string]bool `json:"capabilities"`
	Findings     []hostFinding   `json:"findings"`
}

type hostLint struct {
	HostCommit string              `json:"host_commit"`
	Versions   map[string]hostLine `json:"versions"`
}

type lintCase struct {
	name   string
	sql    string
	golden hostLint
}

func lintCorpus(c *qt.C) []lintCase {
	c.Helper()
	paths, err := filepath.Glob("testdata/statementfacts/*.yql")
	c.Assert(err, qt.IsNil)
	slices.Sort(paths)
	var cases []lintCase
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yql")
		sql, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		data, err := os.ReadFile(strings.TrimSuffix(path, ".yql") + ".golden.json")
		c.Assert(err, qt.IsNil)
		var golden hostLint
		c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
		c.Assert(golden.Versions, qt.HasLen, 3, qt.Commentf("%s", name))
		cases = append(cases, lintCase{name: name, sql: string(sql), golden: golden})
	}
	c.Assert(len(cases) >= 12, qt.IsTrue, qt.Commentf("%d cases", len(cases)))
	return cases
}

// lineColumn is the host's position of a byte offset: the 1-based line, and
// the 1-based column counted in characters.
func lineColumn(sql string, offset int) (int, int) {
	line, column := 1, 1
	for i, r := range sql {
		if i >= offset {
			break
		}
		if r == '\n' {
			line, column = line+1, 1
			continue
		}
		column++
	}
	return line, column
}

// ownedFindings lints one source from the statement facts of the provider's
// split, as the host's lintOwned does, on a target whose capabilities are
// caps. Each finding is rule, position and what it is about: the table of a
// missing primary key, the key of a capability the target lacks, and the
// message otherwise.
func ownedFindings(c *qt.C, sql string, caps map[string]bool) []string {
	c.Helper()
	split, err := migrationlint.SplitRequest(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{{Name: "x.yql", Sql: sql}}}, true)
	c.Assert(err, qt.IsNil)
	examined := map[protocol.LintStatementClass]bool{
		protocol.LintStatementClass_LINT_STATEMENT_CLASS_CREATE_TABLE: slices.Contains(split.Judged, protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY),
		protocol.LintStatementClass_LINT_STATEMENT_CLASS_ALTER_TABLE:  slices.Contains(split.Judged, protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY),
	}
	var findings, unanalyzed []string
	note := func(kind string) {
		if !slices.Contains(unanalyzed, kind) {
			unanalyzed = append(unanalyzed, kind)
		}
	}
	for _, statement := range split.Files[0].Statements {
		line, column := lineColumn(sql, int(statement.Start))
		at := fmt.Sprintf("%d:%d", line, column)
		kinds := slices.DeleteFunc(slices.Clone(statement.Effects), func(effect *protocol.LintEffect) bool {
			return effect.Fact != protocol.LintFact_LINT_FACT_STATEMENT_KIND
		})
		c.Assert(kinds, qt.HasLen, 1)
		kind := kinds[0]
		if kind.StatementClass == protocol.LintStatementClass_LINT_STATEMENT_CLASS_OTHER {
			findings = append(findings, at+" SQL002 ptah sql lint does not lint "+kind.Kind+" statements yet")
			continue
		}
		required := false
		for _, effect := range statement.Effects {
			switch effect.Fact {
			case protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY:
				findings = append(findings, at+" DDL001 "+effect.Table)
			case protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY:
				required = true
				held, answered := caps[effect.Capability]
				switch {
				case held:
				case answered:
					findings = append(findings, at+" CAP001 "+effect.Capability)
				default:
					note("the " + effect.Capability + " requirement")
				}
			}
		}
		if !required && !examined[kind.StatementClass] {
			note(kind.Kind)
		}
	}
	if len(unanalyzed) > 0 {
		findings = append(findings, "1:1 SQL004 no rule examined "+strings.Join(unanalyzed, ", ")+" in this file")
	}
	return findings
}

var (
	builtinMissingKey  = regexp.MustCompile(`^table "(.*)" has no primary key, which YDB refuses`)
	builtinRequirement = regexp.MustCompile(`requires target capability (\S+), unavailable on this target$`)
)

// builtinFindings is the built-in reading's findings in the form of
// ownedFindings.
func builtinFindings(findings []hostFinding) []string {
	result := []string{}
	for _, finding := range findings {
		about := finding.Message
		switch finding.Rule {
		case "DDL001":
			about = builtinMissingKey.FindStringSubmatch(finding.Message)[1]
		case "CAP001":
			about = builtinRequirement.FindStringSubmatch(finding.Message)[1]
		}
		result = append(result, fmt.Sprintf("%d:%d %s %s", finding.Line, finding.Column, finding.Rule, about))
	}
	return result
}

func TestStatementFactsLintAsTheHostsYQLReading(t *testing.T) {
	cases := lintCorpus(qt.New(t))
	for _, test := range cases {
		for version, line := range test.golden.Versions {
			t.Run(test.name+"/"+version, func(t *testing.T) {
				c := qt.New(t)

				got := ownedFindings(c, test.sql, line.Capabilities)

				c.Assert(append([]string{}, got...), qt.DeepEquals, builtinFindings(line.Findings))
			})
		}
	}
}

// TestStatementFactsCorpusReachesEveryRule pins that the corpus holds every
// finding the host's reading reports from the facts, on more than one line,
// and that the goldens come from one host commit.
func TestStatementFactsCorpusReachesEveryRule(t *testing.T) {
	c := qt.New(t)
	rules := map[string]int{}
	commits := map[string]bool{}
	for _, test := range lintCorpus(c) {
		commits[test.golden.HostCommit] = true
		for _, line := range test.golden.Versions {
			for _, finding := range line.Findings {
				rules[finding.Rule]++
			}
		}
	}
	c.Assert(commits, qt.HasLen, 1)
	for _, rule := range []string{"DDL001", "CAP001", "SQL002", "SQL004"} {
		c.Assert(rules[rule] > 1, qt.IsTrue, qt.Commentf("%s: %d", rule, rules[rule]))
	}
}

// testdata/statementfacts/stricter holds the files the two readings answer
// differently on purpose. The built-in reading skips a statement that starts
// with no word and the first statement of a file that opens with a byte order
// mark, so a file of them reads as clean; and it names a table written as a
// named expression with an empty string. The statement facts lint the first
// two and keep the name as written.
func TestStatementFactsAreStricterOnPurpose(t *testing.T) {
	expression := "SQL002 ptah sql lint does not lint expression statements yet"
	for _, test := range []struct {
		file    string
		builtin []string
		owned   []string
	}{
		{file: "expressions", builtin: []string{"2:1 SQL002 ptah sql lint does not lint 1 statements yet"},
			owned: []string{"1:1 " + expression, "2:1 SQL002 ptah sql lint does not lint 1 statements yet", "3:1 " + expression}},
		{file: "bom", builtin: []string{"2:1 SQL002 ptah sql lint does not lint UPSERT statements yet"},
			owned: []string{"1:2 DDL001 bom", "2:1 SQL002 ptah sql lint does not lint UPSERT statements yet"}},
		{file: "named-table",
			builtin: []string{"1:1 SQL002 ptah sql lint does not lint named expression statements yet", "2:1 DDL001 ", "3:1 CAP001 add_column_with_default"},
			owned:   []string{"1:1 SQL002 ptah sql lint does not lint named expression statements yet", "2:1 DDL001 $t", "3:1 CAP001 add_column_with_default"}},
	} {
		t.Run(test.file, func(t *testing.T) {
			c := qt.New(t)
			sql, err := os.ReadFile("testdata/statementfacts/stricter/" + test.file + ".yql")
			c.Assert(err, qt.IsNil)
			data, err := os.ReadFile("testdata/statementfacts/stricter/" + test.file + ".golden.json")
			c.Assert(err, qt.IsNil)
			var golden hostLint
			c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
			line := golden.Versions["25.1.4.7"]

			c.Assert(builtinFindings(line.Findings), qt.DeepEquals, test.builtin)
			c.Assert(ownedFindings(c, string(sql), line.Capabilities), qt.DeepEquals, test.owned)
		})
	}
}

// statementFacts is each annotation's statement facts, or "declined".
func statementFacts(annotations []*protocol.LintAnnotation) []string {
	result := []string{}
	for _, annotation := range annotations {
		described := []string{}
		for _, effect := range annotation.Effects {
			described = append(described, effect.Fact.String()+" "+effect.StatementClass.String()+" "+effect.Kind+" "+effect.Table+effect.Capability)
		}
		result = append(result, strings.Join(append(described, fmt.Sprint(annotation.Declined)), "; "))
	}
	return result
}

// An annotation gives every statement YQL splits the same way its statement
// facts, and declines one YQL splits otherwise: every statement has a kind,
// so the two splits cannot agree about it.
func TestAnnotateDescribesWhatYQLSplitsTheSameWay(t *testing.T) {
	c := qt.New(t)
	sql := "CREATE TABLE t (id Uint64);\nUPSERT INTO t (id) VALUES ('a;b');\n"
	facts := migrationlint.StatementFacts()
	request := &protocol.AnnotateLintRequest{Engine: "ydb", Facts: facts, Sources: []*protocol.LintSource{{Name: "x.sql", Sql: sql}},
		Files: []*protocol.LintFile{{Name: "x.sql", Statements: []*protocol.LintStatement{
			{Start: 0, End: 26, Line: 1, Sql: sql[0:26]},
			{Start: 28, End: 57, Line: 2, Sql: sql[28:57]},
			{Start: 58, End: 61, Line: 2, Sql: sql[58:61]},
		}}}}

	annotated, err := migrationlint.AnnotateRequest(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(statementFacts(annotated.Files[0].Statements), qt.DeepEquals, []string{
		"LINT_FACT_STATEMENT_KIND LINT_STATEMENT_CLASS_CREATE_TABLE CREATE TABLE ; " +
			"LINT_FACT_MISSING_PRIMARY_KEY LINT_STATEMENT_CLASS_UNSPECIFIED  t; false",
		"true",
		"true",
	})
}
