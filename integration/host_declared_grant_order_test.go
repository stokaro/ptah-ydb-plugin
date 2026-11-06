//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `schema inspect --format sql` writes a grant on a table among the common
// statements and the grants on a topic and on a coordination node after
// every other statement, as the built-in YDB engine at host 74de0b65b wrote
// them on 25.1.4.7 and 26.2.1.14. The host sorted all of them by the object
// they are on, so the grant on the topic `events` came before the one on the
// table `t`. It needs a host whose provider render writes the grants on
// declared objects last.
func TestHostInspectsGrantsOnDeclaredObjectsLast_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	group := "ptahdecl" + strings.ToLower(rand.Text())[:8]
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
	})
	for _, statement := range []string{
		"CREATE TABLE `" + absolute("t") + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TOPIC `" + absolute("events") + "`",
		"CREATE GROUP `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), absolute("workers"), coordination.NodeConfig{}), qt.IsNil)
	for _, statement := range []string{
		"GRANT SELECT ROW ON `" + absolute("t") + "` TO `" + group + "`",
		"GRANT DESCRIBE SCHEMA ON `" + absolute("events") + "` TO `" + group + "`",
		"GRANT DESCRIBE SCHEMA ON `" + absolute("workers") + "` TO `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(grantsOn(output, directory), qt.DeepEquals, []string{"t " + group, "events " + group, "workers " + group}, qt.Commentf("%s", output))
}
