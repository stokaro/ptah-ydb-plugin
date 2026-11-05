//go:build integration

package integration_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// largeSchema is a YAML source of n row tables with two columns each.
func largeSchema(n int) string {
	var text strings.Builder
	text.WriteString("tables:\n")
	for i := range n {
		fmt.Fprintf(&text, "  t%03d:\n    columns:\n      id: {type: Uint64, primary: true}\n      v: {type: Utf8}\n", i)
	}
	return text.String()
}

// The host validates a declaration of 40 tables through this provider
// offline. The host's request carries each table's coverage, and every
// coverage record repeats its model's whole definition, so with the native
// field grammar inline in each definition a table cost about 260 KB and a
// schema of 15 tables passed the protocol's 4 MiB message limit
// (`provider validate schema failed (ResourceExhausted)`), where the host's
// embedded engine took any number.
func TestHostValidatesASchemaOfManyTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": largeSchema(40)}), "schema.yaml")

	output, err := hostCommand(c, "schema", "validate", "--dialect", "ydb", "--schema-file", file)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Not(qt.Contains), "ResourceExhausted")
}
