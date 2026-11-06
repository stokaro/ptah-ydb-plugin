//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// builtinExports holds HCL files Ptah's built-in YDB engine exported for a
// directory h1 with a table, a topic and two coordination nodes; its README
// says how they were recorded.
const builtinExports = "../internal/backend/testdata/builtin-hcl"

// builtinExport is the text of the recorded export at name.
func builtinExport(c *qt.C, name string) string {
	c.Helper()
	data, err := os.ReadFile(filepath.Join(builtinExports, filepath.FromSlash(name)))
	c.Assert(err, qt.IsNil)
	return string(data)
}

// builtinRealm is a dev realm whose cleanup removes the directory h1 the
// recorded exports describe, with everything in it, and the lock node a run
// in the realm creates at its root. The realm gives each test its own h1,
// and a run in it takes no lock at the database root.
func builtinRealm(c *qt.C) (address string, opened *connection.Connection, root string) {
	c.Helper()
	opened, _, _ = inspectionFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root = realmFixture(c, opened, base)
	// The realm's directory is made here, so the fixture's cleanup finds it
	// also after a run the host refused before it opened a session.
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), root), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(root, "h1"))
		_ = opened.Driver.Coordination().DropNode(ctx, path.Join(root, locking.NodeName))
	})
	return address, opened, root
}

// hostApplyHCL runs `schema apply` of the HCL documents at address over the
// directory h1, with flags, and returns what the host printed.
func hostApplyHCL(c *qt.C, address string, flags []string, documents ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	arguments := append([]string{"schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", address, "--schemas", "h1"}, flags...)
	directory := c.TempDir()
	for i, document := range documents {
		file := filepath.Join(directory, string(rune('a'+i))+".hcl")
		c.Assert(os.WriteFile(file, []byte(document), 0o600), qt.IsNil)
		arguments = append(arguments, "--schema-file", file)
	}
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, arguments...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// splitExportFlags writes the recorded --out-dir export's files other than
// its schema file and returns them as --schema-file flags.
func splitExportFlags(c *qt.C) []string {
	c.Helper()
	directory := c.TempDir()
	var flags []string
	for _, name := range []string{"split/tables/h1_items.hcl", "split/objects/0003.hcl", "split/objects/0004.hcl"} {
		file := filepath.Join(directory, path.Base(name))
		c.Assert(os.WriteFile(file, []byte(builtinExport(c, name)), 0o600), qt.IsNil)
		flags = append(flags, "--schema-file", file)
	}
	return flags
}

// builtinJobsBlock is the jobs node's block in the recorded export.
var builtinJobsBlock = regexp.MustCompile(`(?s)coordination_node "jobs" \{.*?\n\}\n\n`)

// The HCL files the built-in engine exported read through the host with the
// meaning that engine read them with. Their header claims every coordination
// node of the directory under the built-in engine's model, which this
// provider names as its own model's predecessor. Measured with the built-in
// engine at 74de0b65b on 25.1.4.7 and 26.2.1.14, and the same here: the
// export applied to an empty directory creates the table and both nodes; it,
// the ptah-compat export and the --out-dir files then plan nothing; without
// the jobs block the plan drops jobs; and with the header replaced by a
// ptah:not-described entry for jobs the node is left alone.
func TestHostReadsTheBuiltinEnginesHCLExports_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	address, opened, root := builtinRealm(c)
	inspect := builtinExport(c, "inspect.hcl")
	withoutJobs := builtinJobsBlock.ReplaceAllString(inspect, "")
	unmanaged := `// ptah:not-described coordination_node "h1/jobs"` + "\n" + coverageHeaderLine.ReplaceAllString(withoutJobs, "")

	created, createErr := hostApplyHCL(c, address, nil, inspect)
	held := heldEntries(c, opened, path.Join(root, "h1"))
	again := hostPlannedAt(c, address, "h1", "hcl", inspect)
	compat := hostPlannedAt(c, address, "h1", "hcl", builtinExport(c, "compat-inspect.hcl"))
	split := hostPlannedAt(c, address, "h1", "hcl", builtinExport(c, "split/schemas/h1.hcl"), splitExportFlags(c)...)
	dropped := hostPlannedAt(c, address, "h1", "hcl", withoutJobs)
	kept := hostPlannedAt(c, address, "h1", "hcl", unmanaged)

	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(held, qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE, "jobs": Ydb_Scheme.Entry_COORDINATION_NODE,
		"locks": Ydb_Scheme.Entry_COORDINATION_NODE})
	c.Assert(again, qt.HasLen, 0)
	c.Assert(compat, qt.HasLen, 0)
	c.Assert(split, qt.HasLen, 0)
	c.Assert(withoutJobs, qt.Not(qt.Equals), inspect)
	c.Assert(dropped, qt.DeepEquals, []string{"coordination-node-operation h1.jobs"})
	c.Assert(unmanaged, qt.Not(qt.Contains), "ptah:feature-coverage")
	c.Assert(kept, qt.HasLen, 0)
}

// A header that names the built-in engine's model under an identity that
// engine never wrote, here schema version 2, is not a predecessor the
// provider names, and the host refuses the document before it plans.
func TestHostReadsTheBuiltinEnginesHCLExports_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	address, opened, root := builtinRealm(c)
	inspect := builtinExport(c, "inspect.hcl")
	unknown := strings.Replace(inspect, `"version":1`, `"version":2`, 1)

	output, err := hostApplyHCL(c, address, []string{"--dry-run"}, unknown)

	c.Assert(unknown, qt.Not(qt.Equals), inspect)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `unknown feature codec: "ptah.run/ydb/coordination-node"/desired`)
	c.Assert(heldEntries(c, opened, path.Join(root, "h1")), qt.HasLen, 0)
}
