// Command ydbreplayoracle records what the Ptah host's YDB replay guard
// (internal/devclean/ydbreplay) answers for each statement of the provider's
// corpus under every replay scope, so the provider's guard can be compared
// with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	mkdir -p <host>/cmd/ydbreplayoracle
//	cp internal/yql/testdata/realm/oracle/main.go <host>/cmd/ydbreplayoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbreplayoracle <plugin>/internal/yql/testdata/realm)
//
// It reads statements.txt, one statement per line, with `\n` standing for a
// line break, and writes scopes.json: for each statement and each scope the
// host's verdict and, for a refusal, the operation it names.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ptah.run/internal/devclean/replay"
	"ptah.run/internal/devclean/ydbreplay"
)

type verdict struct {
	Outcome   string `json:"outcome"`
	Operation string `json:"operation,omitempty"`
}

type row struct {
	Statement string             `json:"statement"`
	Scopes    map[string]verdict `json:"scopes"`
}

type golden struct {
	HostCommit string `json:"host_commit"`
	Rows       []row  `json:"rows"`
}

var scopes = map[string]replay.Scope{
	"database":    {},
	"session":     {Session: true},
	"server":      {Server: true},
	"provisioned": {Server: true, Provisioned: true},
	"databases":   {Databases: true},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbreplayoracle <dir>")
		os.Exit(2)
	}
	commit := os.Getenv("HOST_COMMIT")
	if commit == "" {
		fmt.Fprintln(os.Stderr, "HOST_COMMIT is required")
		os.Exit(2)
	}
	file, err := os.Open(filepath.Join(os.Args[1], "statements.txt"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()
	guard := ydbreplay.Services()[0].Guard
	out := golden{HostCommit: commit}
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		statement := strings.ReplaceAll(lines.Text(), `\n`, "\n")
		if strings.TrimSpace(statement) == "" {
			continue
		}
		answer := row{Statement: statement, Scopes: map[string]verdict{}}
		for name, scope := range scopes {
			err := guard.Validate(statement, scope)
			if refusal, ok := errors.AsType[*replay.Refusal](err); ok {
				answer.Scopes[name] = verdict{Outcome: "refused", Operation: refusal.Operation}
				continue
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			answer.Scopes[name] = verdict{Outcome: "accepted"}
		}
		out.Rows = append(out.Rows, answer)
	}
	if err := lines.Err(); err != nil || len(out.Rows) == 0 {
		fmt.Fprintln(os.Stderr, "statements.txt holds no statement", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(os.Args[1], "scopes.json"), append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
