//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from TestRender_FeatureLossNeedsNoTarget. One table holds two
// changefeeds, so DBML reports two omitted feeds even without a target.
// Filtering that table out removes the changefeed warning.
func TestHostDBMLReportsYDBFeatureLossWithoutATarget_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	requireHostChange(c, "provider-defined export omission counts")
	source := writeSource(c, "schema.yaml", `tables:
  plain:
    columns:
      id: {type: Uint64, primary: true}
  events:
    columns:
      id: {type: Uint64, primary: true}
    changefeeds:
      updates: {mode: UPDATES, format: JSON}
      audit: {mode: UPDATES, format: JSON}
  archive:
    columns:
      id: {type: Uint64, primary: true}
`)
	export := func(extra ...string) (string, string) {
		c.Helper()
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
		defer cancel()
		args := []string{"schema", "export", "--to", "dbml", "--schema-file", source,
			"--provider-config", manifest, "--provider-engine", "ydb"}
		args = append(args, extra...)
		cmd := testCommand(ctx, host, args...)
		var document, diagnostics bytes.Buffer
		cmd.Stdout, cmd.Stderr = &document, &diagnostics
		c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", document.String(), diagnostics.String()))
		return document.String(), diagnostics.String()
	}

	document, diagnostics := export()
	c.Assert(document, qt.Contains, `Table "events"`)
	c.Assert(diagnostics, qt.Contains, "DBML cannot express changefeeds (2)")

	selected, selectedDiagnostics := export("--exclude-tables", "events,archive")
	c.Assert(selected, qt.Contains, `Table "plain"`)
	c.Assert(selected, qt.Not(qt.Contains), `Table "events"`)
	c.Assert(selectedDiagnostics, qt.Not(qt.Contains), "DBML cannot express changefeeds")
}
