// Command ydbsqllintoracle records what the Ptah host's built-in YQL reading
// of `ptah sql lint` reports over the YQL files of the provider's
// statement-facts corpus, on each YDB release line the corpus names, so the
// provider's statement facts can be compared with it without importing host
// code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	mkdir -p <host>/cmd/ydbsqllintoracle
//	cp internal/migrationlint/testdata/statementfacts/oracle/main.go <host>/cmd/ydbsqllintoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbsqllintoracle <plugin>/internal/migrationlint/testdata/statementfacts)
//
// For each <case>.yql it writes <case>.golden.json: for every release line,
// the line's answer for each capability a YQL statement can require
// (yqlddl.Requirements), and the findings of sqllint.LintSource with the ydb
// dialect, each with its rule, line, column and message.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ptah.run/core/platform/capability"
	"ptah.run/internal/builtintest"
	"ptah.run/internal/servertarget"
	"ptah.run/internal/sqllint"
)

// versions are the release lines every case is linted against: the oldest
// and the newest line CI runs, and the one line between them whose vector
// indexes do not take bit vectors.
var versions = []string{"25.1.4.7", "25.4.1.15", "26.2.1.14"}

// requirements are the capabilities yqlddl.Requirements names.
var requirements = []capability.Capability{
	capability.UniqueIndexOnExistingTable, capability.AddColumnWithDefault, capability.VectorIndexes, capability.VectorBitType,
}

type finding struct {
	Rule    string `json:"rule"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

type line struct {
	Capabilities map[string]bool `json:"capabilities"`
	Findings     []finding       `json:"findings"`
}

type golden struct {
	HostCommit string          `json:"host_commit"`
	Versions   map[string]line `json:"versions"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbsqllintoracle <corpus>")
		os.Exit(2)
	}
	commit := os.Getenv("HOST_COMMIT")
	if commit == "" {
		fmt.Fprintln(os.Stderr, "HOST_COMMIT is required")
		os.Exit(2)
	}
	paths, err := filepath.Glob(filepath.Join(os.Args[1], "*.yql"))
	if err != nil || len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "the corpus holds no case", err)
		os.Exit(1)
	}
	for _, file := range paths {
		if err := record(file, commit); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			os.Exit(1)
		}
	}
}

func record(file, commit string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	out := golden{HostCommit: commit, Versions: map[string]line{}}
	for _, version := range versions {
		target, err := servertarget.Resolve("ydb", version)
		if err != nil {
			return err
		}
		recorded := line{Capabilities: map[string]bool{}, Findings: []finding{}}
		for _, key := range requirements {
			if target.Capabilities.Established(key) {
				recorded.Capabilities[string(key)] = target.Capabilities.Has(key)
			}
		}
		findings, err := sqllint.LintSource(context.Background(), sqllint.Source{Name: filepath.Base(file), SQL: string(data)},
			sqllint.Options{SQL: builtintest.Runtime().SQL(), Bodies: builtintest.Runtime(), Dialect: "ydb", Version: version})
		if err != nil {
			return err
		}
		for _, f := range findings {
			recorded.Findings = append(recorded.Findings, finding{Rule: f.Rule, Line: f.Line, Column: f.Column, Message: f.Message})
		}
		out.Versions[version] = recorded
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(file, ".yql")+".golden.json", append(encoded, '\n'), 0o644)
}
