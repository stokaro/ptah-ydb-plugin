//go:build integration

package integration_test

import (
	"path"
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests here plan HCL documents through the host with this provider
// selected. An HCL document is an Atlas file, which names none of the
// coordination nodes it leaves out, so it claims the nodes only through its
// header, as the host's embedded YDB engine reads it: a coordination_node
// entry, or the captured ptah:feature-coverage header an export writes. A
// document with neither drops no node.

// coverageHeaderLine matches the captured feature coverage header line of an
// HCL document.
var coverageHeaderLine = regexp.MustCompile(`(?m)^// ptah:feature-coverage \{.*\}\n`)

// hclExportWithClaim exports the directory as HCL through the host and
// returns the document, which declares node. The test is skipped on a host
// that writes the export without a captured feature coverage header: such a
// host takes an HCL document's claim of the nodes from the provider's reply,
// which the SDK makes complete for a document without header limits, so a
// plan from any HCL document drops the nodes it does not declare. Any other
// answer fails the test.
func hclExportWithClaim(c *qt.C, directory, node string) string {
	c.Helper()
	document, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(document, qt.Matches, `(?s).*coordination_node "`+regexp.QuoteMeta(node)+`" \{.*`, qt.Commentf("%s", document))
	if !coverageHeaderLine.MatchString(document) {
		c.Skip("the selected host writes this provider's HCL export without a ptah:feature-coverage header, " +
			"and takes an HCL document's claim of the nodes from the provider's complete reply")
	}
	return document
}

// An HCL document that declares the directory and no node plans nothing over
// a directory that holds one, and one that declares another node only creates
// it: neither claims the nodes it does not name. With a coordination_node
// header entry that names another node, the document claims the rest, and the
// plan drops the node it leaves out.
func TestHostKeepsTheNodesAnHCLDocumentDoesNotName(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	createNode(c, opened, path.Join(database, directory, "plain"))
	hclExportWithClaim(c, directory, "plain")
	empty := `schema "` + directory + `" {}` + "\n"
	another := empty + `coordination_node "` + directory + `" "other" {}` + "\n"
	claimed := `// ptah:not-described coordination_node "` + directory + `/unmanaged"` + "\n" + empty

	c.Assert(hostPlanned(c, directory, "hcl", empty), qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, "hcl", another), qt.DeepEquals, []string{"coordination-node-operation " + directory + ".other"})
	c.Assert(hostPlanned(c, directory, "hcl", claimed), qt.DeepEquals, []string{"coordination-node-operation " + directory + ".plain"})
}

// An HCL export of a directory claims its nodes through its captured header:
// without the header and a node's block it plans nothing, and with the header
// and without the block it drops the node.
func TestHostTakesAnHCLExportsClaimFromItsHeader(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	createNode(c, opened, path.Join(database, directory, "local"))
	document := hclExportWithClaim(c, directory, "local")
	block := regexp.MustCompile(`(?s)coordination_node "local" \{.*?\n\}\n`)
	withoutBlock := block.ReplaceAllString(document, "")
	withoutEither := coverageHeaderLine.ReplaceAllString(withoutBlock, "")

	c.Assert(withoutBlock, qt.Not(qt.Equals), document)
	c.Assert(withoutEither, qt.Not(qt.Equals), withoutBlock)
	c.Assert(hostPlanned(c, directory, "hcl", withoutEither), qt.HasLen, 0, qt.Commentf("%s", withoutEither))
	c.Assert(hostPlanned(c, directory, "hcl", withoutBlock), qt.DeepEquals, []string{"coordination-node-operation " + directory + ".local"},
		qt.Commentf("%s", withoutBlock))
}
