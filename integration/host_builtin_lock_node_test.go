//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A database Ptah's built-in YDB engine locked holds the lock node that
// engine creates, with neither consistency mode set (CreateNode with an empty
// configuration, in its internal/dblockext/ydb). `schema apply` through the
// provider takes its lock on that node, as the engine's runs did, and applies
// the declaration; the node reads and attaches strictly after it.
func TestHostAppliesOnADatabaseTheBuiltInEngineLocked_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	node := path.Join(database, locking.NodeName)
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), node, coordination.NodeConfig{}), qt.IsNil)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": rebuildEntities(directory, "BIGINT")})

	applied, err := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities, "--schemas", directory,
		"--auto-approve", "--tx-mode", "none")
	_, config, describeErr := opened.Driver.Coordination().DescribeNode(c.Context(), node)

	c.Assert(err, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully")
	c.Assert(heldColumnTypes(c, opened, path.Join(database, directory, "items"))["n"], qt.Equals, "Optional<Int64>")
	c.Assert(describeErr, qt.IsNil)
	c.Assert(config.ReadConsistencyMode, qt.Equals, coordination.ConsistencyModeStrict)
	c.Assert(config.AttachConsistencyMode, qt.Equals, coordination.ConsistencyModeStrict)
}
