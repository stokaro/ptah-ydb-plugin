//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// realmTables is how many tables the live test plans: more than a 4 MiB
// message holds once the models travel by reference (about 180), and fewer
// than offline validation takes (manyTables), since the read of each table
// runs within the host's 30-second call to the provider on a shared
// runner. A realm of 500 tables was measured to plan as synced on 25.1.4.7 and
// 26.2.1.14 in 104 to 144 seconds.
const realmTables = 300

// manyTablesRealm is a dev realm of its own holding realmTables tables, each
// keyed by id with one text column, created directly so the test measures the
// plan and not the creation. It returns the realm's address and the source
// that declares those tables.
func manyTablesRealm(c *qt.C) (address, source string) {
	c.Helper()
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	names := make([]string, realmTables)
	for i := range names {
		names[i] = fmt.Sprintf("t%04d", i)
	}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, name := range names {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, name)+"`"), qt.IsNil)
		}
		// The host's schema lock leaves its node at the realm's root.
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(root, locking.NodeName))
	})
	var declared strings.Builder
	declared.WriteString("tables:\n")
	for start := 0; start < len(names); start += 50 {
		var statements strings.Builder
		for _, name := range names[start:min(start+50, len(names))] {
			fmt.Fprintf(&statements, "CREATE TABLE `%s` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id));\n", path.Join(root, name))
			fmt.Fprintf(&declared, "  %s:\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      body: {type: Utf8}\n", name)
		}
		c.Assert(execFixtureSchema(ctx, opened.DB, statements.String()), qt.IsNil)
	}
	return address, declared.String()
}

// A realm of realmTables tables plans as synced in one run, as the built-in
// engine planned it: every model of each table's coverage travels by reference, and
// the requests that carry what the read found in each table pass 4 MiB, which
// the host and the provider negotiate. Without the two features the host
// refused to send the table preparation (`provider prepare tables failed
// (ResourceExhausted)`). The dry run takes no schema lock, so the test
// measures the plan alone.
func TestHostPlansARealmOfManyTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.LargeMessagesFeature)
	address, source := manyTablesRealm(c)
	schema := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(schema, []byte(source), 0o600), qt.IsNil)

	output, err := hostRunAt(c, address, "schema", "apply", "--dry-run", "--schema-file", schema)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema is synced, no changes to be made")
}
