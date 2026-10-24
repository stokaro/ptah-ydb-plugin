//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// hostEvents declares events in the fixture directory, keyed by (ts, id), with
// the given further attributes of its table directive.
func hostEvents(directory, attributes string) string {
	return "package entities\n\n//ptah:schema:table name=\"events\" schema=\"" + directory + "\" primary_key=\"ts,id\"" + attributes + "\n" +
		"type Event struct {\n\t//ptah:schema:field name=\"ts\" type=\"Timestamp\" primary=\"true\"\n\tTS int64\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody string\n}\n"
}

// hostColumnTable is events as a column table hashed by id in three shards,
// whose rows are deleted a day after ts.
const hostColumnTable = ` store="COLUMN" partition_by_hash="id" column_shards="3" platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`

// hostColumnFixture is the fixture directory, with events dropped when the
// test ends. The host takes the provider's schema lock on every run, whose node
// the lock fixture removes again.
func hostColumnFixture(c *qt.C) string {
	c.Helper()
	monitoredAddress(c)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	return directory
}

// The host creates a column table from a Go source through the provider, finds
// it as declared on the next run, and changes its TTL in place. The key the Go
// fields leave nullable is created NOT NULL, as the host's embedded engine
// creates it, and the read table matches the declaration.
func TestHostAppliesAColumnTable_HappyPath(t *testing.T) {
	c := qt.New(t)
	directory := hostColumnFixture(c)
	const twoDays = ` store="COLUMN" partition_by_hash="id" column_shards="3" platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P2D"`
	for _, run := range []struct{ name, attributes, want string }{
		{"creation", hostColumnTable, `(?s).*column-table-create-operation.*Schema apply completed successfully.*`},
		{"a second run", hostColumnTable, `(?s).*No changes planned.*`},
		{"another TTL", twoDays, `(?s).*column-table-ttl-operation.*Schema apply completed successfully.*`},
		{"a run after the TTL", twoDays, `(?s).*No changes planned.*`},
	} {
		output, err := hostApply(c, directory, hostEvents(directory, run.attributes))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
}

// What YDB fixes when it creates a column table is refused with the reason
// when a later declaration changes it.
func TestHostRefusesAColumnTableChange_FailurePath(t *testing.T) {
	c := qt.New(t)
	directory := hostColumnFixture(c)
	output, err := hostApply(c, directory, hostEvents(directory, hostColumnTable))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	for _, test := range []struct{ name, attributes, want string }{
		{"another shard count", ` store="COLUMN" partition_by_hash="id" column_shards="4" platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`,
			`changing a column table's hash key or shard count requires an explicit data migration \(the table has 3 shards, and the declaration asks for 4\)`},
		{"another hash key", ` store="COLUMN" partition_by_hash="ts,id" column_shards="3" platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`,
			`changing a column table's hash key or shard count requires an explicit data migration`},
		{"a row table", ` platform.ydb.row_deletion_column="ts" platform.ydb.row_deletion_interval="P1D"`,
			`changing between row and column storage requires an explicit data migration`},
	} {
		output, err := hostApply(c, directory, hostEvents(directory, test.attributes))
		c.Assert(err, qt.IsNotNil, qt.Commentf("%s: %s", test.name, output))
		c.Assert(output, qt.Matches, `(?s).*`+test.want+`.*`, qt.Commentf("%s", test.name))
	}
}
