//go:build integration

package integration_test

import (
	"os"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `schema apply` prints its plan under the heading the built-in YDB engine at
// host 74de0b65b printed above every plan, `Planned schema changes:`, measured
// on 25.1.4.7 and 26.2.1.14 for a table, a topic, a coordination node and a
// secret, in a dry run and in an apply. A plan through this provider holds
// native operations, and the host printed those under `Planned changes:`.
func TestHostApplyPrintsTheBuiltInsPlanHeading_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _ := rehearsalFixture(c)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rehearsalEntities(directory, "")})

	previewed, previewErr := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", entities, "--schemas", directory, "--dry-run")
	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", entities, "--schemas", directory, "--auto-approve")

	c.Assert(previewErr, qt.IsNil, qt.Commentf("%s", previewed))
	c.Assert(previewed, qt.Contains, "Planned schema changes:\nCREATE TABLE `"+directory+"/items`")
	c.Assert(previewed, qt.Not(qt.Contains), "Planned changes:")
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Contains, "Planned schema changes:\nCREATE TABLE `"+directory+"/items`")
	c.Assert(applied, qt.Not(qt.Contains), "Planned changes:")
}
