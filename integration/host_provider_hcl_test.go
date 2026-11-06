//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// providerExports holds an HCL file this provider exported through the host,
// built as release 0.4.0, for the directory h1 the built-in engine's exports
// describe; its README says how it was recorded.
const providerExports = "../internal/backend/testdata/provider-hcl"

// providerExport is the text of the recorded export at name.
func providerExport(c *qt.C, name string) string {
	c.Helper()
	data, err := os.ReadFile(filepath.Join(providerExports, filepath.FromSlash(name)))
	c.Assert(err, qt.IsNil)
	return string(data)
}

// providerCoordinationDigest matches the definition digest the recorded
// header names the coordination-node model by.
var providerCoordinationDigest = regexp.MustCompile(
	`("kind":"github\.com/stokaro/ptah-ydb-plugin/coordination-node","representation":"desired","version":1,"definition":"sha256:)[0-9a-f]{64}"`)

// An HCL export an earlier build of this provider wrote reads through the
// host with the build under test. Its header names each model by the
// identity the provider describes, which this build describes too or names
// as a predecessor: the export applied to an empty directory creates the
// table and both nodes, read again plans nothing, and without the jobs block
// the plan drops jobs, as the header claims every coordination node of h1.
func TestHostReadsAnEarlierProviderHCLExport_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	address, opened, root := builtinRealm(c)
	inspect := providerExport(c, "inspect.hcl")
	withoutJobs := builtinJobsBlock.ReplaceAllString(inspect, "")

	created, createErr := hostApplyHCL(c, address, nil, inspect)
	held := heldEntries(c, opened, path.Join(root, "h1"))
	again := hostPlannedAt(c, address, "h1", "hcl", inspect)
	dropped := hostPlannedAt(c, address, "h1", "hcl", withoutJobs)

	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(held, qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE, "jobs": Ydb_Scheme.Entry_COORDINATION_NODE,
		"locks": Ydb_Scheme.Entry_COORDINATION_NODE})
	c.Assert(again, qt.HasLen, 0)
	c.Assert(withoutJobs, qt.Not(qt.Equals), inspect)
	c.Assert(dropped, qt.DeepEquals, []string{"coordination-node-operation h1.jobs"})
}

// A header that names the coordination-node model by a definition the
// provider neither describes nor names as a predecessor, as an export of a
// release before a definition text changed does until the provider names the
// identity it replaced, is refused before the host plans.
func TestHostReadsAnEarlierProviderHCLExport_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	address, opened, root := builtinRealm(c)
	inspect := providerExport(c, "inspect.hcl")
	unknown := providerCoordinationDigest.ReplaceAllString(inspect, "${1}"+strings.Repeat("0", 64)+`"`)

	output, err := hostApplyHCL(c, address, []string{"--dry-run"}, unknown)

	c.Assert(unknown, qt.Not(qt.Equals), inspect)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `incompatible feature codec: coverage model "github.com/stokaro/ptah-ydb-plugin/coordination-node"/desired`)
	c.Assert(heldEntries(c, opened, path.Join(root, "h1")), qt.HasLen, 0)
}
