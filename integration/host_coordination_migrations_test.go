//go:build integration

package integration_test

import (
	"os"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBBinary_GeneratesAndAppliesCoordinationNodes:
// the two ways a coordination node declared in Go reaches a database.
// `migrations generate` writes the provider's statement into the up and the
// down file, `migrations up` runs it, `schema compare --exit-code` finds
// nothing left to change, `db read` describes the node, and `migrations
// down` drops it. Then `schema apply` creates it directly, its preview
// showing the provider's statement, and a second apply plans nothing.
func TestHostGeneratesAndAppliesCoordinationNodes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	address := os.Getenv("PTAH_YDB_TEST_URL")
	entities := writeFiles(c, c.TempDir(), map[string]string{"nodes.go": strings.ReplaceAll("package entities\n\n//ptah:schema:table name=\"jobs\" schema=\"{dir}\"\n"+
		"type Job struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n}\n\n"+
		"//ptah:schema:coordinationnode name=\"limits\" schema=\"{dir}\" self_check_period=\"PT2S\" read_consistency_mode=\"strict\"\ntype Limits struct{}\n", "{dir}", directory)})
	migrations := c.TempDir()
	scope := []string{"--db-url", address, "--schemas", directory}
	target := []string{"--db-url", address, "--migrations-dir", migrations, "--migrations-schema", directory}
	node := path.Join(database, directory, "limits")
	create := "CREATE COORDINATION NODE `" + directory + "/limits` WITH (self_check_period = Interval('PT2S'), read_consistency_mode = 'strict');"

	generated, generateErr := hostCommand(c, append([]string{"migrations", "generate", "--root-dir", entities, "--migrations-dir", migrations, "--name", "nodes"}, scope...)...)
	c.Assert(generateErr, qt.IsNil, qt.Commentf("generate:\n%s", generated))
	up, down := generatedPair(c, migrations)
	c.Assert(up, qt.Contains, create)
	c.Assert(down, qt.Contains, "DROP COORDINATION NODE `"+directory+"/limits`;")

	applied, upErr := hostCommand(c, append([]string{"migrations", "up"}, target...)...)
	compared, compareErr := hostCommand(c, append([]string{"schema", "compare", "--root-dir", entities, "--exit-code"}, scope...)...)
	read, readErr := hostCommand(c, append([]string{"db", "read"}, scope...)...)
	_, served, servedErr := opened.Driver.Coordination().DescribeNode(c.Context(), node)
	rolledBack, downErr := hostCommand(c, append([]string{"migrations", "down", "--target", "0", "--confirm"}, target...)...)
	_, _, goneErr := opened.Driver.Coordination().DescribeNode(c.Context(), node)

	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(readErr, qt.IsNil, qt.Commentf("db read:\n%s", read))
	c.Assert(read, qt.Contains, create)
	c.Assert(servedErr, qt.IsNil)
	c.Assert(*served, qt.DeepEquals, coordination.NodeConfig{SelfCheckPeriodMillis: 2000, ReadConsistencyMode: coordination.ConsistencyModeStrict})
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", rolledBack))
	c.Assert(goneErr, qt.ErrorMatches, `(?s).*SCHEME_ERROR.*`)

	first, firstErr := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", entities, "--schemas", directory, "--auto-approve", "--tx-mode", "none")
	second, secondErr := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", entities, "--schemas", directory, "--auto-approve", "--tx-mode", "none")
	_, _, appliedErr := opened.Driver.Coordination().DescribeNode(c.Context(), node)

	c.Assert(firstErr, qt.IsNil, qt.Commentf("first apply:\n%s", first))
	c.Assert(first, qt.Contains, create)
	c.Assert(secondErr, qt.IsNil, qt.Commentf("second apply:\n%s", second))
	c.Assert(second, qt.Contains, "Schema is synced, no changes to be made")
	c.Assert(appliedErr, qt.IsNil)
}
