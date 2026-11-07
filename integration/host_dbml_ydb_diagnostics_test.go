//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The former host TestSchemaInspectReport_DBMLReportsYDBObjectsOnDiagnostics
// requires the DBML document to omit YDB-only objects and warn about each
// omitted family. This source-only export uses the published host and this
// provider without requiring a database target.
func TestHostDBMLReportsYDBObjectsOnDiagnostics(t *testing.T) {
	c := testcheck.New(t)
	directory := writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": `tables:
  t:
    columns:
      id: {type: Int64, primary: true}
secrets:
  credentials:
    value_env: PTAH_SECRET_CREDENTIALS
async_replications:
  mirror:
    connection_string: grpc://primary.example.com:2136/?database=/prod
    items:
      - source: t
        target: r
`})
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "export", "--to", "dbml", "--schema-file", filepath.Join(directory, "schema.yaml"),
		"--provider-config", manifest, "--provider-engine", "ydb")
	var document, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &document, &diagnostics
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", document.String(), diagnostics.String()))
	c.Assert(document.String(), qt.Contains, `Table "t" {`)
	c.Assert(document.String(), qt.Not(qt.Contains), "warning:")
	c.Assert(document.String(), qt.Not(qt.Contains), "credentials")
	c.Assert(document.String(), qt.Not(qt.Contains), "mirror")
	c.Assert(diagnostics.String(), qt.Equals,
		"warning: DBML cannot express YDB async replications (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB secrets (1); the export leaves them out\n")
}
