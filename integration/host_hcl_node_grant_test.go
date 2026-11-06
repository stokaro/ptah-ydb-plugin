//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hclNodeGrant declares the node workers in the directory {dir} and grants
// DESCRIBE SCHEMA on it to the group {group}.
const hclNodeGrant = `schema "{dir}" {
}

coordination_node "workers" {
  schema = schema.{dir}
}

permission {
  to = role.{group}
  for = coordination_node.{dir}.workers
  privileges = ["YDB.GRANULAR.DESCRIBE_SCHEMA"]
}
`

// hclGrantGroup creates a group through db for a test, dropped when the test
// ends.
func hclGrantGroup(c *qt.C, db *sql.DB) string {
	c.Helper()
	group := "ptahhcl" + strings.ToLower(rand.Text())[:8]
	c.Assert(execFixtureSchema(c.Context(), db, "CREATE GROUP `"+group+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, db, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
	})
	return group
}

// A permission block on a coordination node plans its GRANT, and an HCL
// export of the node writes the permission back, as the YDB engine built into
// the host at 74de0b65b planned and wrote them. Through this provider the
// plan held the node alone, and the export left the permission out among the
// details it cannot represent.
func TestHostPlansAndExportsAnHCLGrantOnANode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	group := hclGrantGroup(c, opened.DB)
	document := strings.NewReplacer("{dir}", directory, "{group}", group).Replace(hclNodeGrant)
	grant := "GRANT 'ydb.granular.describe_schema' ON `" + directory + "/workers` TO `" + group + "`"
	file := compatFile(c, c.TempDir(), "schema.hcl", document)

	planned := hostPlanned(c, directory, "hcl", document)
	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--schema-file", file,
		"--auto-approve", "--tx-mode", "none")
	exported, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	replanned := hostPlanned(c, directory, "hcl", document)

	c.Assert(planned, qt.DeepEquals, []string{"coordination-node-operation " + directory + ".workers", grant})
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(exported, qt.Contains, "  for = coordination_node."+directory+".workers\n", qt.Commentf("%s", exported))
	c.Assert(replanned, qt.HasLen, 0)
}

// ptah-compat plans the GRANT a permission block on a coordination node asks
// for, as the built-in engine did. Through this provider it refused the
// document: `a YDB grant cannot name object kind "coordination_node"`.
func TestHostCompatPlansAnHCLGrantOnANode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	group := hclGrantGroup(c, realm.opened.DB)
	document := strings.NewReplacer("{dir}", "app", "{group}", group).Replace(hclNodeGrant)
	desired := "file://" + compatFile(c, c.TempDir(), "schema.hcl", document)

	stdout, stderr, exit := runCompat(c, nil, "schema", "apply", "--url", realm.address, "--to", desired, "--dry-run")

	c.Assert(exit, qt.Equals, 0, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout, stderr))
	c.Assert(previewStatements(stdout), qt.DeepEquals, []string{
		"CREATE COORDINATION NODE `app/workers`;", "GRANT 'ydb.granular.describe_schema' ON `app/workers` TO `" + group + "`;",
	}, qt.Commentf("%s", stdout))
}

// previewStatements lists the lines of a plan preview that start a
// statement, in order, leaving out the headings and the comments a host
// writes above a statement.
func previewStatements(output string) []string {
	var statements []string
	for line := range strings.SplitSeq(output, "\n") {
		if slices.ContainsFunc([]string{"CREATE ", "ALTER ", "DROP ", "GRANT ", "REVOKE "}, func(verb string) bool { return strings.HasPrefix(line, verb) }) {
			statements = append(statements, line)
		}
	}
	return statements
}
