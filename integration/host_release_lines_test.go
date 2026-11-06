//go:build integration

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// releaseLineProfile is the part of `ptah db capabilities --format json` the
// release-line tests read.
type releaseLineProfile struct {
	Preset struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"capability_preset"`
	Certification struct {
		Level string `json:"level"`
		Line  string `json:"line"`
		Label string `json:"label"`
	} `json:"certification"`
	Capabilities []struct {
		Key       string `json:"key"`
		Supported bool   `json:"supported"`
	} `json:"capabilities"`
	Refinements []struct {
		Key string `json:"key"`
	} `json:"configuration_refinements"`
}

// hostKeys are facts of a YDB server the host plans with on every line, which
// no session states and the release line's facts carry.
var hostKeys = []string{"alter_column_drop_not_null", "async_indexes", "column_families", "index_covering_columns", "rename_table", "row_deletion_policy"}

// `ptah db capabilities` certifies a YDB server against the release line this
// provider declares for its version, as the built-in engine at host 74de0b65b
// certified it against the host's YDB cells on 25.1.4.7 and 26.2.1.14: the
// line, the certified level and a version-ladder preset, with the facts the
// host plans with on every line supported. Without the declared lines the
// report was best-effort on no line, every fact a session states listed as a
// refinement of an empty preset, and the host's own facts unsupported.
func TestHostReportsTheReleaseLineOfTheServer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	opened, _, _ := inspectionFixture(c)
	line := serverLine(c, serverVersion(c, opened))

	output, err := runHost(c, host, "db", "capabilities", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--format", "json",
		"--provider-config", manifest, "--provider-engine", "ydb")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	var profile releaseLineProfile
	c.Assert(json.Unmarshal([]byte(output), &profile), qt.IsNil, qt.Commentf("%s", output))
	c.Assert(profile.Certification.Line, qt.Equals, line)
	c.Assert(profile.Certification.Level, qt.Equals, "certified")
	c.Assert(profile.Preset.Source, qt.Equals, "version-ladder")
	// The preset's name is the one the built-in engine gave it, YDB251 for
	// 25.1, and the line has no label of its own.
	c.Assert(profile.Preset.Name, qt.Equals, "YDB"+strings.ReplaceAll(line, ".", ""))
	c.Assert(profile.Certification.Label, qt.Equals, "")
	supported := make(map[string]bool, len(profile.Capabilities))
	for _, entry := range profile.Capabilities {
		supported[entry.Key] = entry.Supported
	}
	for _, key := range hostKeys {
		c.Assert(supported[key], qt.IsTrue, qt.Commentf("%s", key))
	}
	refined := make([]string, 0, len(profile.Refinements))
	for _, entry := range profile.Refinements {
		refined = append(refined, entry.Key)
	}
	c.Assert(refined, qt.Not(qt.Contains), "topics")
	c.Assert(refined, qt.Not(qt.Contains), "coordination_nodes")
}

// A --server-version newer than every line this provider declares is planned
// with the newest line, and one older than every line with the oldest, with
// the warning the built-in engine at host 74de0b65b printed; the provider path
// refused both as naming no declared line.
func TestHostPlansAServerVersionPastTheDeclaredLines_HappyPath(t *testing.T) {
	for _, test := range []struct{ version, warning string }{
		{"26.3.1.18", "warning: ydb 26.3.1.18 is newer than the newest measured release line 26.2; capabilities were planned as 26.2\n"},
		{"24.4.4.12", "warning: ydb 24.4.4.12 is not a measured release line; capabilities fall back to the preset its ladder assigns (newest measured line: 26.2)\n"},
	} {
		t.Run(test.version, func(t *testing.T) {
			c := testcheck.New(t)
			host, manifest := hostManifest(c)
			schema := filepath.Join(c.TempDir(), "schema.yaml")
			c.Assert(os.WriteFile(schema, []byte("tables:\n  release_items:\n    columns:\n      id: {type: Uint64, primary: true}\n"), 0o600), qt.IsNil)

			output, err := runHost(c, host, "schema", "render", "--dialect", "ydb", "--server-version", test.version, "--schema-file", schema,
				"--provider-config", manifest, "--provider-engine", "ydb")

			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.warning)
			c.Assert(output, qt.Contains, "CREATE TABLE `release_items`")
		})
	}
}
