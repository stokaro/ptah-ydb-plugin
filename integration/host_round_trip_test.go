//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// roundTripColumn is one column of the round trip's accounts table: its YAML
// spelling, and the capability the line needs for it, empty for none. A
// column whose type or default a line lacks is left out there, so the round
// trip runs on every line and covers each type where it exists.
type roundTripColumn struct {
	yaml, needs string
}

// roundTripAccounts are the columns of accounts: a Serial key and every type
// and kind of literal default the host's round trip declares.
var roundTripAccounts = []roundTripColumn{
	{yaml: `id: {type: BIGINT, primary: true, auto_increment: true}`},
	{yaml: `email: {type: "VARCHAR(255)", not_null: true}`},
	{yaml: `status: {type: "VARCHAR(20)", default: active}`},
	{yaml: `visits: {type: INTEGER, default: "0"}`},
	{yaml: `verified: {type: BOOLEAN, default: "false"}`},
	{yaml: `created_at: {type: TIMESTAMP, default: "2026-01-02 03:04:05"}`},
	{yaml: `birthday: {type: DATE, default: "2000-02-29"}`},
	{yaml: `lifetime: {type: INTERVAL, default: PT26H}`},
	{yaml: `balance: {type: "DECIMAL(10,2)", default: "12.50"}`, needs: "parameterized_decimal"},
	{yaml: `ratio: {type: REAL, default: "0.10"}`},
	{yaml: `score: {type: DOUBLE PRECISION, default: "1e21"}`},
	{yaml: `profile: {type: JSONB, default: "{\"a\":1}"}`, needs: "document_type_defaults"},
	{yaml: `raw_json: {type: JSON}`},
	{yaml: `avatar: {type: BYTEA, default: raw}`},
	{yaml: `token: {type: UUID, default: "550E8400-E29B-41D4-A716-446655440000"}`},
	{yaml: `tiny: {type: TINYINT, default: "-5"}`},
	{yaml: `small: {type: SMALLINT, default: "5"}`, needs: "small_integer_defaults"},
	{yaml: `u8: {type: TINYINT UNSIGNED, default: "200"}`},
	{yaml: `u16: {type: SMALLINT UNSIGNED}`},
	{yaml: `u32: {type: INT UNSIGNED, default: "7"}`},
	{yaml: `u64: {type: BIGINT UNSIGNED, default: "18446744073709551615"}`},
	{yaml: `dy: {type: DyNumber, default: "3.14"}`, needs: "document_type_defaults"},
	{yaml: `doc: {type: Yson}`},
	{yaml: `n_date: {type: Date, default: "2026-01-02"}`},
	{yaml: `n_datetime: {type: Datetime}`},
	{yaml: `n_ts: {type: Timestamp, default: "2026-01-02T03:04:05.123456Z"}`},
	{yaml: `n_iv: {type: Interval, default: P1D}`},
	{yaml: `w_dt64: {type: Datetime64, default: "1900-01-02T03:04:05Z"}`, needs: "wide_date_time_types"},
}

// roundTripSchema is the host's representative schema in YAML for a line
// with capabilities: accounts with every column the line takes and unique,
// asynchronous and covering indexes; orders with a composite key; events in
// a nested directory; and a table and a column whose names need escaping.
func roundTripSchema(directory string, capabilities map[string]bool) string {
	var accounts []string
	for _, column := range roundTripAccounts {
		if column.needs == "" || capabilities[column.needs] {
			accounts = append(accounts, "      "+column.yaml)
		}
	}
	return strings.ReplaceAll("tables:\n  accounts:\n    schema: {dir}\n    columns:\n"+strings.Join(accounts, "\n")+"\n"+
		"    indexes:\n      uq_accounts_email: {fields: [email], unique: true}\n      idx_accounts_status: {fields: [status], type: async}\n"+
		"      idx_accounts_created: {fields: [created_at], include: [status, visits]}\n"+
		"  orders:\n    schema: {dir}\n    primary_key: [tenant, id]\n    columns:\n      tenant: {type: TEXT, not_null: true}\n      id: {type: BIGINT, not_null: true}\n"+
		"      amount: {type: \"DECIMAL(22,9)\", default: \"1.5\"}\n      note: {type: TEXT}\n    indexes:\n      idx_orders_amount: {fields: [amount]}\n"+
		"  events:\n    schema: {dir}/archive\n    columns:\n      id: {type: BIGINT, primary: true}\n      payload: {type: TEXT}\n"+
		"  \"tick`name.with.dot\":\n    schema: {dir}\n    columns:\n      id: {type: INTEGER, primary: true}\n      weird-col: {type: TEXT}\n", "{dir}", directory)
}

// builtTable is what the server built of a table: its key, each column as
// name, type and whether its default draws from a sequence, and each index
// as its name, kind, key and covered columns.
type builtTable struct {
	Key     []string
	Columns map[string]string
	Indexes []string
}

func describeBuilt(c *qt.C, opened *connection.Connection, absolute string) builtTable {
	c.Helper()
	built := builtTable{Columns: map[string]string{}}
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		built.Key = described.PrimaryKey
		for _, column := range described.Columns {
			text := column.Type.Yql()
			if column.DefaultValue != nil && column.DefaultValue.Sequence() != nil {
				text += " serial"
			}
			built.Columns[column.Name] = text
		}
		for _, index := range described.Indexes {
			kind := map[options.IndexType]string{options.IndexTypeGlobal: "GLOBAL", options.IndexTypeGlobalAsync: "GLOBAL_ASYNC",
				options.IndexTypeGlobalUnique: "GLOBAL_UNIQUE"}[index.Type]
			built.Indexes = append(built.Indexes, index.Name+" "+kind+" ("+strings.Join(index.IndexColumns, ",")+") cover ("+
				strings.Join(index.DataColumns, ",")+")")
		}
		slices.Sort(built.Indexes)
		return nil
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	return built
}

// Ported from the host's TestYDBRoundTrip_NothingLeftToPlan and
// TestYDBRoundTrip_ReadsWhatTheServerBuilt: the representative schema, every
// type the line takes with a literal default of each kind, a Serial key, a
// composite key, unique, asynchronous and covering indexes, a table in a
// nested directory, and a table and a column whose names need escaping,
// applies through the host and the next two runs plan nothing. The server
// holds the Serial key on a sequence, the composite key in its order, each
// index of its kind with its covered columns, and the escaped names. The
// host's test reads the defaults through its reader; the provider's reading
// of them is what the empty replans measure.
func TestHostAppliesTheRoundTripSchema_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Cleanup(func() {
		// emptyTree quotes a name as it is, and a backquote in the name
		// ends the quoted identifier, so this table goes first, escaped.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "tick\\`name.with.dot")+"`"), qt.IsNil)
	})
	selectors := directory + "," + directory + "/archive"

	applyUntilSynced(c, selectors, "yaml", roundTripSchema(directory, targetCapabilities(c)))

	accounts := describeBuilt(c, opened, path.Join(database, directory, "accounts"))
	c.Assert(accounts.Key, qt.DeepEquals, []string{"id"})
	c.Assert(accounts.Columns["id"], qt.Equals, "Int64 serial")
	c.Assert(accounts.Columns["email"], qt.Equals, "Utf8")
	c.Assert(accounts.Indexes, qt.DeepEquals, []string{
		"idx_accounts_created GLOBAL (created_at) cover (status,visits)",
		"idx_accounts_status GLOBAL_ASYNC (status) cover ()",
		"uq_accounts_email GLOBAL_UNIQUE (email) cover ()",
	})
	orders := describeBuilt(c, opened, path.Join(database, directory, "orders"))
	c.Assert(orders.Key, qt.DeepEquals, []string{"tenant", "id"})
	c.Assert(orders.Columns["amount"], qt.Equals, "Optional<Decimal(22,9)>")
	c.Assert(describeBuilt(c, opened, path.Join(database, directory, "archive", "events")).Key, qt.DeepEquals, []string{"id"})
	tick := describeBuilt(c, opened, path.Join(database, directory, "tick`name.with.dot"))
	c.Assert(tick.Columns["weird-col"], qt.Equals, "Optional<Utf8>")
}
