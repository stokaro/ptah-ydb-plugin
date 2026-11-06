//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// defaultPoolStatements are the lines of output that name the resource pool
// default.
func defaultPoolStatements(output string) []string {
	var named []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "RESOURCE POOL `default`") {
			named = append(named, line)
		}
	}
	return named
}

// `schema inspect --schemas <dir> --format sql` writes the pool default as
// the built-in YDB engine at host 74de0b65b wrote it, on 25.1.4.7 and
// 26.2.1.14 with resource pools on: nothing while the pool sets nothing, and
// the ALTER RESOURCE POOL ... SET of what it sets once it sets something. The
// provider wrote `CREATE RESOURCE POOL default WITH (CONCURRENT_QUERY_LIMIT =
// "-1")` and `CREATE RESOURCE POOL default WITH (...)`, a statement YDB
// refuses for a pool the database holds. A read of one directory reports the
// database's pools only to a host that lists schema.database-kinds.
func TestHostInspectsTheDefaultPoolAsTheBuiltInEngineDid_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DatabaseKindsFeature)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	items := path.Join(database, directory, "items")
	createSchemaObject(c, opened, "CREATE TABLE `"+items+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+items+"`")
	inspect := func() []string {
		output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")
		c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
		c.Assert(output, qt.Contains, "CREATE TABLE `"+directory+"/items`", qt.Commentf("%s", output))
		return defaultPoolStatements(output)
	}

	c.Assert(inspect(), qt.HasLen, 0)

	createSchemaObject(c, opened, "ALTER RESOURCE POOL `default` SET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 50)",
		"ALTER RESOURCE POOL `default` RESET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE)")
	c.Assert(inspect(), qt.DeepEquals, []string{"ALTER RESOURCE POOL `default` SET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 50);"})
}

// The statement inspect writes for the pool default runs: applied to the
// database it sets what the pool holds again, where the provider's CREATE
// RESOURCE POOL default failed on the pool the database holds.
func TestHostRunsTheDefaultPoolStatementInspectWrites_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DatabaseKindsFeature)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	items := path.Join(database, directory, "items")
	createSchemaObject(c, opened, "CREATE TABLE `"+items+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+items+"`")
	createSchemaObject(c, opened, "ALTER RESOURCE POOL `default` SET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 50)",
		"ALTER RESOURCE POOL `default` RESET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE)")
	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	statements := defaultPoolStatements(output)
	c.Assert(statements, qt.HasLen, 1, qt.Commentf("%s", output))
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, opened.DB, "ALTER RESOURCE POOL `default` RESET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE)"), qt.IsNil)

	c.Assert(execFixtureSchema(ctx, opened.DB, strings.TrimSuffix(statements[0], ";")), qt.IsNil)

	c.Assert(readPoolSettings(c, opened.DB, database, "default").QueryMemoryLimitPercentPerNode, qt.Equals, float64(50))
}
