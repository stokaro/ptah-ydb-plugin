//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
)

// The host's selectors name native objects within the opened database, while
// inspected envelopes retain that database as provenance. This read-only diff
// checks that both sides of the public host path use the same selector identity.
func TestHostScopesNativeObjectsSubprocess(t *testing.T) {
	c := qt.New(t)
	host, manifest := hostManifest(c)
	opened, directory, database := inspectionFixture(c)
	for _, name := range []string{"workers", "unchanged"} {
		node := path.Join(database, directory, name)
		c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), node, coordination.NodeConfig{SelfCheckPeriodMillis: 2000}), qt.IsNil)
		c.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.Check(opened.Driver.Coordination().DropNode(ctx, node), qt.IsNil)
		})
	}
	inspect := exec.CommandContext(c.Context(), host, "schema", "inspect", "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--format", "json")
	var inspectDiagnostics bytes.Buffer
	inspect.Stderr = &inspectDiagnostics
	observed, err := inspect.Output()
	c.Assert(err, qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", observed, inspectDiagnostics.String()))
	var document any
	c.Assert(json.Unmarshal(observed, &document), qt.IsNil)
	if !hasCapturedHostFeature(document, "scope.selections") {
		c.Skip("the selected host did not negotiate scope.selections")
	}
	source := filepath.Join(c.TempDir(), "schema.yaml")
	text := fmt.Sprintf("coordination_nodes:\n  workers:\n    schema: %s\n    self_check_period: PT3S\n  unchanged:\n    schema: %s\n    self_check_period: PT2S\n", directory, directory)
	c.Assert(os.WriteFile(source, []byte(text), 0o600), qt.IsNil)
	for _, test := range []struct {
		name, flag, selector string
		statements           []string
	}{
		{"included node", "--include", directory + ".workers[type=coordination_node]", []string{"ALTER COORDINATION NODE `" + directory + "/workers` SET (self_check_period = Interval('PT3S'))"}},
		{"excluded node", "--exclude", directory + ".workers[type=coordination_node]", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, host, "schema", "diff", "--provider-config", manifest, "--provider-engine", "ydb", "--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", source, "--schemas", directory, "--format", "json", test.flag, test.selector)
			var diagnostics bytes.Buffer
			command.Stderr = &diagnostics
			output, err := command.Output()
			c.Assert(err, qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", output, diagnostics.String()))
			var result struct {
				Statements []string `json:"statements"`
			}
			c.Assert(json.Unmarshal(output, &result), qt.IsNil)
			c.Assert(result.Statements, qt.DeepEquals, test.statements)
		})
	}
}

// The provider envelope records the features actually negotiated by the host.
// Read that evidence instead of inferring support from a version number.
func hasCapturedHostFeature(value any, feature string) bool {
	switch node := value.(type) {
	case map[string]any:
		if _, envelope := node["engine"]; envelope {
			if features, ok := node["features"].([]any); ok && slices.Contains(features, any(feature)) {
				return true
			}
		}
		for _, child := range node {
			if hasCapturedHostFeature(child, feature) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if hasCapturedHostFeature(child, feature) {
				return true
			}
		}
	}
	return false
}
