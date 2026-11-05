//go:build integration

package integration_test

import (
	"fmt"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// runIntervalItems declares table items keyed on an unsigned id, with a
// column n of the type given and a TTL after a day on a Timestamp column ts.
func runIntervalItems(directory, nType string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory +
		"\" platform.ydb.row_deletion_column=\"ts\" platform.ydb.row_deletion_interval=\"P1D\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" not_null=\"true\" primary=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"" + nType + "\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"ts\" type=\"Timestamp\"\n\tTS int64\n}\n"
}

// runIntervalFixture is items created through the host with n as Int32 and a
// row, its TTL then set to run every half hour through the table service,
// which is the only way to write a run interval: YQL has no setting for one.
func runIntervalFixture(c *qt.C) (opened *connection.Connection, directory, absolute string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	created, err := hostApplySource(c, directory, "go", runIntervalItems(directory, "Int32"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n, ts) VALUES (1ul, 7, CurrentUtcTimestamp())"), qt.IsNil)
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	c.Assert(session.Alter(c.Context(), &Ydb_Table.AlterTableRequest{Path: absolute, TtlAction: &Ydb_Table.AlterTableRequest_SetTtlSettings{SetTtlSettings: &Ydb_Table.TtlSettings{
		Mode:               &Ydb_Table.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "ts", ExpireAfterSeconds: 86400}},
		RunIntervalSeconds: 1800,
	}}}), qt.IsNil)
	c.Assert(heldTTL(c, opened, absolute), qt.Equals, "ts 86400s every 1800s")
	return opened, directory, absolute
}

// heldTTL is the TTL the table at absolute holds as its column, its interval
// and its run interval, each in seconds, read through the table service, or
// empty for none. YDB reports a run interval of 0 for its default.
func heldTTL(c *qt.C, opened *connection.Connection, absolute string) string {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	ttl := described.GetTtlSettings().GetDateTypeColumn()
	if ttl == nil {
		return ""
	}
	return fmt.Sprintf("%s %ds every %ds", ttl.GetColumnName(), ttl.GetExpireAfterSeconds(), described.GetTtlSettings().GetRunIntervalSeconds())
}

// Ported from the host's TestYDBRebuild_RefusesATTLRunInterval. The host's
// built-in engine refuses to rebuild a table whose TTL runs on an interval of
// its own, since recreating the table would drop the interval. This provider
// creates the new table through the table service, which writes one: `schema
// apply --allow-table-rebuild` rebuilds the table for a column type change,
// the new table keeps the declared TTL, the run interval and the row, and
// nothing is left to plan.
func TestHostRebuildKeepsATTLRunInterval_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := runIntervalFixture(c)

	rebuildRuns(c, directory, "go", runIntervalItems(directory, "Int64"))

	c.Assert(heldTTL(c, opened, absolute), qt.Equals, "ts 86400s every 1800s")
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(rowCount(c, opened, absolute, "n = 7"), qt.Equals, int64(1))
}

// A migration file cannot carry the run interval, since YQL's TTL setting has
// none: `migrations generate --allow-table-rebuild` refuses the forward
// rebuild, whose new table keeps the interval, says why, writes no file, and
// the table keeps its interval.
func TestHostRebuildKeepsATTLRunInterval_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := runIntervalFixture(c)
	migrations := c.TempDir()

	refused, err := hostCommand(c, "migrations", "generate", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"),
		"--root-dir", writeFiles(c, c.TempDir(), map[string]string{"entities.go": runIntervalItems(directory, "Int64")}), "--schemas", directory,
		"--migrations-dir", migrations, "--name", "rebuild", "--allow-table-rebuild")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "error: error assessing migration safety: unsupported feature: native operations cannot be represented as SQL: "+
		"the ydb provider has no text form for operation github.com/stokaro/ptah-ydb-plugin/row-table-create-operation (Creates a row table): "+
		"no YQL text form: the TTL runs on an interval of its own, and YQL's TTL setting has none")
	up, down := generatedPair(c, migrations)
	c.Assert(up+down, qt.Equals, "")
	c.Assert(heldTTL(c, opened, absolute), qt.Equals, "ts 86400s every 1800s")
	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int32>")
}
