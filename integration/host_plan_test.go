//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		Kind         string `json:"kind"`
		Subject      string `json:"subject"`
		Severity     string `json:"severity"`
		Reason       string `json:"reason"`
		Access       string `json:"access"`
		AccessReason string `json:"access_reason"`
	} `json:"native"`
}

// unitPrefix starts the line appliedUnits writes for each native operation.
const unitPrefix = "Planned unit: "

// appliedUnits writes the units of an apply's --json report, one line each,
// ahead of what the host printed, so a test can read a plan's native
// operations whatever the host's preview shows for them: a host may list a
// native operation by its kind, or show the provider's statement for it. A
// native operation is its kind, the object it changes, and its assessment:
//
//	Planned unit: <kind> <subject>: <reason> (<severity>); access <access>: <access reason>
//
// A statement is its first line. The report itself follows what the host
// printed, so nothing it says, such as why a run failed, is lost; a report
// that is not JSON lists no unit.
func appliedUnits(report []byte, printed string) string {
	var decoded struct {
		Units []plannedUnit `json:"units"`
	}
	if json.Unmarshal(report, &decoded) != nil {
		return printed + string(report)
	}
	var out strings.Builder
	for _, unit := range decoded.Units {
		if unit.SQL != "" {
			out.WriteString(unitPrefix + unit.head() + "\n")
			continue
		}
		native := unit.Native
		fmt.Fprintf(&out, "%s%s %s: %s (%s); access %s: %s\n", unitPrefix, native.Kind, native.Subject, native.Reason, native.Severity, native.Access, native.AccessReason)
	}
	return out.String() + printed + string(report)
}

// pluginUnits lists the native operations of this provider appliedUnits
// listed in output, in plan order, each as the last segment of its kind and
// the object it changes.
func pluginUnits(output string) []string {
	var listed []string
	for line := range strings.SplitSeq(output, "\n") {
		unit, found := strings.CutPrefix(line, unitPrefix+"github.com/stokaro/ptah-ydb-plugin/")
		if !found {
			continue
		}
		head, _, _ := strings.Cut(unit, ":")
		listed = append(listed, head)
	}
	return listed
}

// unitsSaying counts the units appliedUnits listed whose line holds text,
// such as the reason of an operation's assessment.
func unitsSaying(output, text string) int {
	count := 0
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, unitPrefix) && strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// unitCount counts the native operations of kind appliedUnits listed.
func unitCount(output, kind string) int {
	return strings.Count(output, unitPrefix+kind+" ")
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
	units := hostPlannedUnitsAt(c, address, selectors, format, text, flags...)
	heads := make([]string, len(units))
	for i, unit := range units {
		heads[i] = unit.head()
	}
	return heads
}

// hostPlannedUnits is hostPlanned's plan as the host reports each unit, with
// the assessment of a native operation.
func hostPlannedUnits(c *qt.C, selectors, format, text string, flags ...string) []plannedUnit {
	c.Helper()
	return hostPlannedUnitsAt(c, os.Getenv("PTAH_YDB_TEST_URL"), selectors, format, text, flags...)
}

// hostPlannedUnitsAt is hostPlannedUnits against address.
func hostPlannedUnitsAt(c *qt.C, address, selectors, format, text string, flags ...string) []plannedUnit {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	name := map[string]string{"go": "entities.go", "yaml": "schema.yaml", "sql": "schema.sql", "hcl": "schema.hcl", "dbml": "schema.dbml"}[format]
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
	cmd := testCommand(ctx, host, arguments...)
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
	return report.Units
}
