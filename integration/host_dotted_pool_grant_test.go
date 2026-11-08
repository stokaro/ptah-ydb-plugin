//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A resource pool belongs to the database, so the dot in its name is a
// literal character. The host must keep the provider's grant target name
// through its SQL schema model and render the grant on that same pool. This
// ports the dotted-pool row of the host's TestReadYQLPrivileges. YDB keeps
// resource pools behind EnableResourcePools, so the test turns it on: a
// grant on a pool of a server without them is refused for the capability.
func TestHostRendersYQLGrantOnDottedResourcePool_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, "stokaro/ptah#5777, which keeps the name of a database-wide grant target")
	opened, _, _ := inspectionFixture(c)
	enableResourcePools(c, opened)
	const base = "CREATE USER etl;\n" +
		"CREATE RESOURCE POOL `batch.v2` WITH (CONCURRENT_QUERY_LIMIT=5);\n"
	const grant = "GRANT SELECT ROW ON `.metadata/workload_manager/pools/batch.v2` TO etl;\n"
	files := writeFiles(c, c.TempDir(), map[string]string{"from.sql": base, "to.sql": base + grant})

	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.sql"), "--to", filepath.Join(files, "to.sql"),
		"--dev-url", os.Getenv("PTAH_YDB_TEST_URL"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "GRANT 'ydb.granular.select_row' ON `.metadata/workload_manager/pools/batch.v2` TO `etl`")
}
