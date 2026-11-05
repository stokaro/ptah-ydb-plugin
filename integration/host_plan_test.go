//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	qt "github.com/frankban/quicktest"
)

// plannedUnit is one unit of the plan the host reports for `schema apply
// --dry-run --json`: the statement of a common object, or a native operation
// of this provider with the object it changes.
type plannedUnit struct {
	SQL    string `json:"sql"`
	Native struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
	} `json:"native"`
}

// head is how a test names a unit: the first line of its statement without
// the comments above it and without its terminator, or the last segment of
// the native operation's kind and the object it changes.
func (u plannedUnit) head() string {
	if u.SQL == "" {
		return u.Native.Kind[strings.LastIndex(u.Native.Kind, "/")+1:] + " " + u.Native.Subject
	}
	lines := strings.Split(u.SQL, "\n")
	for len(lines) > 1 && strings.HasPrefix(lines[0], "--") {
		lines = lines[1:]
	}
	return strings.TrimSuffix(lines[0], ";")
}

// hostPlanned runs the host's `schema apply --dry-run --json` over one source
// with this provider selected for ydb, restricted to the comma-separated
// selectors and with the further flags given, and returns the head of each
// unit it would run, in order, or none for a plan with nothing to run. The
// run changes nothing.
func hostPlanned(c *qt.C, selectors, format, text string, flags ...string) []string {
	c.Helper()
	return hostPlannedAt(c, os.Getenv("PTAH_YDB_TEST_URL"), selectors, format, text, flags...)
}

// hostPlannedAt is hostPlanned against address, such as a dev realm's. With
// no selectors the run reads every directory of the address's database.
func hostPlannedAt(c *qt.C, address, selectors, format, text string, flags ...string) []string {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	name := map[string]string{"go": "entities.go", "yaml": "schema.yaml", "sql": "schema.sql", "hcl": "schema.hcl"}[format]
	c.Assert(os.WriteFile(filepath.Join(root, name), []byte(text), 0o600), qt.IsNil)
	source := []string{"--schema-file", filepath.Join(root, name)}
	if format == "go" {
		source = []string{"--root-dir", root}
	}
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	arguments := []string{"schema", "apply", "--dry-run", "--json", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", address}
	if selectors != "" {
		arguments = append(arguments, "--schemas", selectors)
	}
	arguments = append(arguments, slices.Concat(flags, source)...)
	cmd := exec.CommandContext(ctx, host, arguments...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s\n%s", stdout.String(), stderr.String()))
	var report struct {
		Outcome string        `json:"outcome"`
		Units   []plannedUnit `json:"units"`
	}
	c.Assert(json.Unmarshal(stdout.Bytes(), &report), qt.IsNil, qt.Commentf("%s", stdout.String()))
	// A plan with nothing to run reports no-changes and no units.
	c.Assert(report.Outcome, qt.Equals, map[bool]string{true: "dry-run", false: "no-changes"}[len(report.Units) != 0])
	heads := make([]string, len(report.Units))
	for i, unit := range report.Units {
		heads[i] = unit.head()
	}
	return heads
}
