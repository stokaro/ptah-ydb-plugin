//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// applyYQLUntilSynced applies a YQL schema through the host, requires output
// to match planned, and requires the next run to find nothing to change.
func applyYQLUntilSynced(c *qt.C, directory, text, planned, name string) {
	c.Helper()
	root := writeSchema(c, map[string]string{"schema.sql": replaceDirectory(text, directory)})
	schemaFile := filepath.Join(root, "schema.sql")
	output, err := hostApplySQL(c, directory, schemaFile)
	c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
	c.Assert(output, qt.Matches, planned, qt.Commentf("%s: %s", name, output))
	again, err := hostApplySQL(c, directory, schemaFile)
	c.Assert(err, qt.IsNil, qt.Commentf("%s, again: %s", name, again))
	c.Assert(again, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s: %s", name, again))
}

// dropTablesAtCleanup drops the tables at absolutes once the test ends.
func dropTablesAtCleanup(c *qt.C, opened *connection.Connection, absolutes ...string) {
	c.Helper()
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, absolute := range absolutes {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
		}
	})
}

// The host's TestYDBDesiredYQL_AppliesAndSettles. A YQL table with literal
// defaults, a covering index with AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2
// and KEY_BLOOM_FILTER = ENABLED is created, and the next run finds nothing to
// change: the defaults, the index, its setting and the table setting survive
// the read back. The table setting reaches the host as the row-table facet of
// the table, which needs a host that lists schema.sql-facets, and the index
// setting as the index's settings facet, which needs one that also lists
// schema.observed-index-facets.
func TestHostAppliesAYQLTableWithSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.SQLFacetsFeature)
	requireHostNegotiates(c, provider.ObservedIndexFacetsFeature)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	dropTablesAtCleanup(c, opened, absolute)

	applyYQLUntilSynced(c, directory, "CREATE TABLE `{dir}/items` (id Int64 NOT NULL, body Utf8 DEFAULT 'active'u, n Int64 DEFAULT 42l, PRIMARY KEY (id), "+
		"INDEX by_body GLOBAL SYNC ON (body) COVER (n) WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2)) WITH (KEY_BLOOM_FILTER = ENABLED);",
		`(?s).*row-table-create-operation.*Schema apply completed successfully.*`, "creation")

	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, storedTable{Families: []string{"default compression=off"},
		Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0", KeyBloomFilter: "ENABLED"})
	var indexes []string
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		for _, index := range described.Indexes {
			indexes = append(indexes, index.Name+" on "+index.IndexColumns[0]+" cover "+index.DataColumns[0])
		}
		return err
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	c.Assert(indexes, qt.DeepEquals, []string{"by_body on body cover n"})
	c.Assert(heldSettings(c, opened, absolute)["by_body"], qt.Matches, `min=2 .*`)
}

// The host's TestYDBDesiredYQL_TTLAndColumnFamilies. A YQL table declares a
// TTL and puts a column in a family of its own; both apply, and the next run
// is synced. Removing only the TTL from the schema resets the TTL and leaves
// the table and its family as they are. The TTL and the family reach the host
// as the row-table facet of the table, and the column's family as a facet of
// the column.
func TestHostAppliesYQLTTLAndColumnFamilies_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.SQLFacetsFeature)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	dropTablesAtCleanup(c, opened, absolute)
	const createTable = "CREATE TABLE `{dir}/events` (id Int64 NOT NULL, ts Timestamp, body Utf8 FAMILY payload, PRIMARY KEY (id), FAMILY payload (COMPRESSION = 'lz4'))"
	families := []string{"default compression=off", "payload compression=lz4 columns=body"}
	for _, step := range []struct {
		name, suffix, planned string
		want                  storedTable
	}{
		{"a TTL and a family", " WITH (TTL = Interval('PT1H') ON ts);", `(?s).*row-table-create-operation.*Schema apply completed successfully.*`,
			storedTable{TTL: "ts 3600s", Families: families, Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0"}},
		{"the TTL removed", ";", `(?s).*row-table-alter-operation.*Schema apply completed successfully.*`,
			storedTable{Families: families, Partitioning: "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0"}},
	} {
		applyYQLUntilSynced(c, directory, createTable+step.suffix, step.planned, step.name)
		c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, step.want, qt.Commentf("%s", step.name))
	}
}

// The YQL spelling of TestHostAppliesAColumnTable_HappyPath. A YQL column
// table hashed by id in three shards, whose rows are deleted a day after ts, is
// created, found as declared on the next run, and its TTL changed in place.
// The storage reaches the host as the column-storage facet a YAML or Go
// source declares for the same table.
func TestHostAppliesAYQLColumnTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.SQLFacetsFeature)
	directory := hostColumnFixture(c)
	const createTable = "CREATE TABLE `{dir}/events` (ts Timestamp NOT NULL, id Uint64 NOT NULL, body Utf8, PRIMARY KEY (ts, id)) PARTITION BY HASH (id) " +
		"WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, TTL = Interval('%s') ON ts);"
	for _, step := range []struct{ name, interval, planned string }{
		{"creation", "P1D", `(?s).*column-table-create-operation.*Schema apply completed successfully.*`},
		{"another TTL", "P2D", `(?s).*column-table-ttl-operation.*Schema apply completed successfully.*`},
	} {
		applyYQLUntilSynced(c, directory, fmt.Sprintf(createTable, step.interval), step.planned, step.name)
	}
}
