//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests in this file run what the provider's YD rules judge on the server
// they judge it for, and lint the same statements through the provider's lint
// service against what a session on that server established: its version and
// its capabilities, the server's feature flags among them. Each test then
// reads the server's answer back, so a rule that says the server refuses a
// statement, or changes something the statement does not name, is held to what
// the server does.

// lintSession is a provider session on the test database and the client that
// opened it, with the code of every rule the provider publishes.
type lintSession struct {
	client  *providerruntime.Client
	session *protocol.Session
	codes   []string
}

// newLintSession opens a session on the test database. A session reads the
// server's flags when it opens, so a test that changes a flag opens its
// session after the change.
func newLintSession(c *qt.C) lintSession {
	c.Helper()
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	var codes []string
	for _, rule := range client.Description().Engines[0].LintRules {
		codes = append(codes, rule.Code)
	}
	c.Assert(codes, qt.Not(qt.HasLen), 0)
	return lintSession{client: client, session: session, codes: codes}
}

// line is the release line of the session's server, such as 26.2.
func (s lintSession) line(c *qt.C) string {
	c.Helper()
	return serverLine(c, s.session.ServerVersion)
}

// reported lints a directory of two versions, the setup and then the statement
// under test, with every rule the provider publishes, against the session's
// server version and capabilities. It returns the rules reported on the
// statement under test.
func (s lintSession) reported(c *qt.C, setup []string, statement string) []string {
	c.Helper()
	var first strings.Builder
	for _, step := range setup {
		first.WriteString(step + ";\n")
	}
	split, err := s.client.SplitLintStatements(c.Context(), &protocol.SplitLintRequest{Engine: "ydb", Sources: []*protocol.LintSource{
		{Name: "0000000001_setup.up.sql", Sql: first.String(), Version: 1, Up: true},
		{Name: "0000000002_change.up.sql", Sql: statement + ";\n", Version: 2, Up: true},
	}})
	c.Assert(err, qt.IsNil)
	c.Assert(split.Files, qt.HasLen, 2)
	checked, err := s.client.CheckLintMigrations(c.Context(), &protocol.CheckLintRequest{
		Engine:        "ydb",
		ServerVersion: s.session.ServerVersion,
		Capabilities:  s.session.Capabilities,
		Codes:         s.codes,
		Files: []*protocol.LintFile{
			{Name: "0000000001_setup.up.sql", Version: 1, Up: true, Statements: split.Files[0].Statements},
			{Name: "0000000002_change.up.sql", Version: 2, Up: true, Statements: split.Files[1].Statements},
		},
	})
	c.Assert(err, qt.IsNil)
	rules := make([]string, 0)
	for _, finding := range checked.Findings {
		if finding.File == 1 {
			rules = append(rules, finding.Code)
		}
	}
	return rules
}

// runSetup runs each setup statement, and fails the test on the first the
// server refuses.
func runSetup(c *qt.C, opened *connection.Connection, setup []string) {
	c.Helper()
	for _, step := range setup {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, step), qt.IsNil, qt.Commentf("setup: %s", step))
	}
}

// replaced is each statement with {dir} replaced by dir.
func replaced(statements []string, dir string) []string {
	result := make([]string, 0, len(statements))
	for _, statement := range statements {
		result = append(result, strings.ReplaceAll(statement, "{dir}", dir))
	}
	return result
}

// errorText is an error's text, or the empty string for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// usedColumnsTable has a column an index keys, one it covers, one the TTL
// reads, and one nothing uses.
const usedColumnsTable = "CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, k Utf8, c Utf8, ts Timestamp, free Utf8, " +
	"PRIMARY KEY (id), INDEX t_k GLOBAL SYNC ON (k) COVER (c)) WITH (TTL = Interval(\"P1D\") ON ts)"

// Ported from the host's TestYDBLint_RulesReportWhatTheServerRefuses: each
// statement here is one a YD rule judges, run on the server the lint judges it
// for. The rule reports exactly the statements the server refuses, and the
// refusal is the one the rule's message quotes. Where the two lines differ,
// the row says what each answers, and an empty answer is a statement the line
// runs. The 26.2 test server starts with EnableAddUniqueIndex on, so it builds
// a unique index on a table that exists and its session establishes
// unique_index_on_existing_table; TestLintUniqueIndexRuleFollowsTheFlag
// measures that line with the flag off.
func TestLintRulesReportWhatTheServerRefuses(t *testing.T) {
	tests := []struct {
		name      string
		setup     []string
		statement string
		rule      string
		refusals  map[string]string
	}{
		{
			name:      "a unique index added to an existing table",
			setup:     []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))"},
			statement: "ALTER TABLE `{dir}/t` ADD INDEX t_v GLOBAL UNIQUE SYNC ON (v)",
			rule:      "YD101",
			refusals:  map[string]string{"26.2": "", "25.1": "Unknown index type: syncGlobalUnique"},
		},
		{
			name:      "a unique index declared with its table",
			statement: "CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id), INDEX t_v GLOBAL UNIQUE SYNC ON (v))",
			rule:      "YD101",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
		{
			name:  "a block that writes a row and creates a table",
			setup: []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))"},
			statement: "DO BEGIN UPSERT INTO `{dir}/t` (id) VALUES (1ul); " +
				"CREATE TABLE `{dir}/m` (id Uint64 NOT NULL, PRIMARY KEY (id)); END DO",
			rule: "YD102",
			refusals: map[string]string{
				"26.2": "Queries with mixed data and scheme operations are not supported",
				"25.1": "Queries with mixed data and scheme operations are not supported",
			},
		},
		{
			name:      "a block whose SELECT reads no table",
			statement: "DO BEGIN SELECT 1; CREATE TABLE `{dir}/m` (id Uint64 NOT NULL, PRIMARY KEY (id)); END DO",
			rule:      "YD102",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
		{
			name:      "a NOT NULL column without a default",
			setup:     []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))"},
			statement: "ALTER TABLE `{dir}/t` ADD COLUMN a Int64 NOT NULL",
			rule:      "YD103",
			refusals: map[string]string{
				"26.2": "Cannot add not null column without default value",
				"25.1": "Cannot add not null column without default value",
			},
		},
		{
			name:      "a column with a default",
			setup:     []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))"},
			statement: "ALTER TABLE `{dir}/t` ADD COLUMN a Int64 NOT NULL DEFAULT 7",
			rule:      "YD103",
			refusals:  map[string]string{"26.2": "", "25.1": "Column addition with default value is not supported now"},
		},
		{
			name:      "a nullable column",
			setup:     []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))"},
			statement: "ALTER TABLE `{dir}/t` ADD COLUMN a Int64",
			rule:      "YD103",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
		{
			name:      "the key column of an index, dropped",
			setup:     []string{usedColumnsTable},
			statement: "ALTER TABLE `{dir}/t` DROP COLUMN k",
			rule:      "YD104",
			refusals: map[string]string{
				"26.2": "Impossible drop column because table has an index with that column",
				"25.1": "Impossible drop column because table has an index with that column",
			},
		},
		{
			name:      "a covered column, dropped",
			setup:     []string{usedColumnsTable},
			statement: "ALTER TABLE `{dir}/t` DROP COLUMN c",
			rule:      "YD104",
			refusals: map[string]string{
				"26.2": "Impossible drop column because table index covers that column",
				"25.1": "Impossible drop column because table index covers that column",
			},
		},
		{
			name:      "the TTL column, dropped",
			setup:     []string{usedColumnsTable},
			statement: "ALTER TABLE `{dir}/t` DROP COLUMN ts",
			rule:      "YD104",
			refusals: map[string]string{
				"26.2": "Can't drop TTL column: 'ts', disable TTL first",
				"25.1": "Can't drop TTL column: 'ts', disable TTL first",
			},
		},
		{
			name:      "the TTL column, dropped after RESET (TTL)",
			setup:     []string{usedColumnsTable, "ALTER TABLE `{dir}/t` RESET (TTL)"},
			statement: "ALTER TABLE `{dir}/t` DROP COLUMN ts",
			rule:      "YD104",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
		{
			name:      "a column no index or TTL uses, dropped",
			setup:     []string{usedColumnsTable},
			statement: "ALTER TABLE `{dir}/t` DROP COLUMN free",
			rule:      "YD104",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
		{
			name: "a table carrying a changefeed, renamed",
			setup: []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))",
				"ALTER TABLE `{dir}/t` ADD CHANGEFEED feed WITH (MODE = 'UPDATES', FORMAT = 'JSON')"},
			statement: "ALTER TABLE `{dir}/t` RENAME TO `{dir}/u`",
			rule:      "YD109",
			refusals: map[string]string{
				"26.2": "Cannot move table with cdc streams",
				"25.1": "Cannot move table with cdc streams",
			},
		},
		{
			name: "a table renamed after its changefeed was dropped",
			setup: []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, PRIMARY KEY (id))",
				"ALTER TABLE `{dir}/t` ADD CHANGEFEED feed WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
				"ALTER TABLE `{dir}/t` DROP CHANGEFEED feed"},
			statement: "ALTER TABLE `{dir}/t` RENAME TO `{dir}/u`",
			rule:      "YD109",
			refusals:  map[string]string{"26.2": "", "25.1": ""},
		},
	}
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	lint := newLintSession(c)
	line := lint.line(c)
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			dir := path.Join(database, directory, fmt.Sprintf("rule_%d", i))
			setup := replaced(test.setup, dir)
			statement := strings.ReplaceAll(test.statement, "{dir}", dir)
			runSetup(c, opened, setup)
			refusal, measured := test.refusals[line]
			c.Assert(measured, qt.IsTrue, qt.Commentf("no answer recorded for YDB %s", line))

			reported := lint.reported(c, setup, statement)
			err := execFixtureSchema(c.Context(), opened.DB, statement)

			c.Assert(errorText(err) != "", qt.Equals, refusal != "", qt.Commentf("YDB %s answered %v", line, err))
			c.Assert(errorText(err), qt.Matches, `(?s).*`+regexp.QuoteMeta(refusal)+`.*`)
			c.Assert(slices.Contains(reported, test.rule), qt.Equals, refusal != "",
				qt.Commentf("lint reported %v for a statement YDB %s answered with %v", reported, line, err))
		})
	}
}

// Ported from the 26.2 row of the host's
// TestYDBLint_RulesReportWhatTheServerRefuses for a unique index added to an
// existing table, which the host measured with EnableAddUniqueIndex off. With
// the flag off, a new session lacks unique_index_on_existing_table, YD101
// reports the index, and the server refuses it in the words the rule's
// documentation quotes. A line whose session lacks the capability with the
// flags the server started with is a row of
// TestLintRulesReportWhatTheServerRefuses, and this test skips there.
func TestLintUniqueIndexRuleFollowsTheFlag(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	requireSessionCapability(c, newLintSession(c).session, tablecreate.UniqueIndexOnExistingTableCapability)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableAddUniqueIndex": false})
	lint := newLintSession(c)
	tablePath := path.Join(database, directory, "t")
	setup := []string{"CREATE TABLE `" + tablePath + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))"}
	runSetup(c, opened, setup)
	statement := "ALTER TABLE `" + tablePath + "` ADD INDEX t_v GLOBAL UNIQUE SYNC ON (v)"

	reported := lint.reported(c, setup, statement)
	err := execFixtureSchema(c.Context(), opened.DB, statement)

	c.Assert(lint.session.Capabilities[tablecreate.UniqueIndexOnExistingTableCapability], qt.IsFalse)
	c.Assert(reported, qt.Contains, "YD101")
	c.Assert(err, qt.ErrorMatches, `(?s).*Adding a unique index to an existing table is disabled.*`)
}

// Ported from the host's TestYDBLint_DroppedOrRenamedTableLeavesItsViewFailing:
// YDB runs a DROP TABLE of a table a view reads, and an ALTER TABLE ... RENAME
// TO of it, and keeps the view, which reads its table by path and then fails
// on every read. YD106 reports both, and the server shows what it reports: the
// statement runs, and the read through the view fails, naming the table's old
// path.
func TestLintDroppedOrRenamedTableLeavesItsViewFailing(t *testing.T) {
	setup := []string{
		"CREATE TABLE `{dir}/base` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE VIEW `{dir}/v` WITH (security_invoker = TRUE) AS SELECT id FROM `{dir}/base`",
	}
	tests := []struct {
		name      string
		statement string
	}{
		{name: "dropped", statement: "DROP TABLE `{dir}/base`"},
		{name: "renamed", statement: "ALTER TABLE `{dir}/base` RENAME TO `{dir}/moved`"},
	}
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	lint := newLintSession(c)
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			dir := path.Join(database, directory, fmt.Sprintf("view_%d", i))
			steps := replaced(setup, dir)
			statement := strings.ReplaceAll(test.statement, "{dir}", dir)
			runSetup(c, opened, steps)

			reported := lint.reported(c, steps, statement)
			runErr := execFixtureSchema(c.Context(), opened.DB, statement)
			var count uint64
			readErr := opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+dir+"/v`").Scan(&count)

			c.Assert(reported, qt.Contains, "YD106")
			c.Assert(runErr, qt.IsNil)
			c.Assert(readErr, qt.ErrorMatches, `(?s).*Cannot find table 'db\.\[`+regexp.QuoteMeta(dir)+`/base\]'.*`)
		})
	}
}

// describedPartitioning is the partitioning settings of the table at absolute
// as DescribeTable reports them, without the provider.
func describedPartitioning(c *qt.C, opened *connection.Connection, absolute string) options.PartitioningSettings {
	c.Helper()
	var settings options.PartitioningSettings
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		settings = described.PartitioningSettings
		return err
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	return settings
}

// Ported from the host's TestYDBLint_PartitioningChangeResetsTheMinimum: YD105
// reports the ALTER TABLE that resets a table's minimum partition count, and
// the count the server keeps afterwards, read back through DescribeTable, is
// what the rule says it is. A table whose minimum the directory left at 1
// loses nothing, and the rule stays silent there.
func TestLintPartitioningChangeResetsTheMinimum(t *testing.T) {
	const minimumOfFour = "AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4, AUTO_PARTITIONING_BY_SIZE = DISABLED, " +
		"AUTO_PARTITIONING_BY_LOAD = DISABLED"
	tests := []struct {
		name     string
		with     string
		set      string
		reported bool
		minimum  uint64
	}{
		{name: "auto partitioning by size turned on", with: minimumOfFour,
			set: "SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)", reported: true, minimum: 1},
		{name: "auto partitioning by load turned on, without parentheses", with: minimumOfFour,
			set: "SET AUTO_PARTITIONING_BY_LOAD ENABLED", reported: true, minimum: 1},
		{name: "turned on with the minimum in the same statement", with: minimumOfFour,
			set: "SET (AUTO_PARTITIONING_BY_LOAD = ENABLED), SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4)", minimum: 4},
		{name: "a size setting", with: minimumOfFour, set: "SET (AUTO_PARTITIONING_PARTITION_SIZE_MB = 100)", minimum: 4},
		{name: "turned on for a table created with four uniform partitions",
			with: "UNIFORM_PARTITIONS = 4, AUTO_PARTITIONING_BY_SIZE = DISABLED",
			set:  "SET (AUTO_PARTITIONING_BY_LOAD = ENABLED)", reported: true, minimum: 1},
		{name: "turned on for a table whose minimum is already 1",
			with: "AUTO_PARTITIONING_BY_SIZE = DISABLED, AUTO_PARTITIONING_BY_LOAD = DISABLED",
			set:  "SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)", minimum: 1},
	}
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	lint := newLintSession(c)
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			name := path.Join(database, directory, fmt.Sprintf("partitions_%d", i))
			create := "CREATE TABLE `" + name + "` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (" + test.with + ")"
			runSetup(c, opened, []string{create})
			statement := "ALTER TABLE `" + name + "` " + test.set

			reported := lint.reported(c, []string{create}, statement)
			c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil)

			c.Assert(slices.Contains(reported, "YD105"), qt.Equals, test.reported, qt.Commentf("lint reported %v", reported))
			c.Assert(describedPartitioning(c, opened, name).MinPartitionsCount, qt.Equals, test.minimum)
		})
	}
}

// Ported from the host's TestYDBLint_PartitioningChangeResetsTheSize: YD118
// reports the ALTER TABLE that resets a table's partition size, and the size
// the server keeps afterwards, read back through DescribeTable, is what the
// rule says it is.
func TestLintPartitioningChangeResetsTheSize(t *testing.T) {
	tests := []struct {
		name     string
		set      string
		reported bool
		size     uint64
	}{
		{name: "auto partitioning by size turned on again", set: "SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)", reported: true, size: 2048},
		{name: "turned on beside the minimum",
			set: "SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 6, AUTO_PARTITIONING_BY_SIZE = ENABLED)", reported: true, size: 2048},
		{name: "turned on with the size in the same statement",
			set: "SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 100)", size: 100},
		{name: "auto partitioning by load turned on", set: "SET (AUTO_PARTITIONING_BY_LOAD = ENABLED)", size: 100},
	}
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	lint := newLintSession(c)
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			name := path.Join(database, directory, fmt.Sprintf("sizes_%d", i))
			create := "CREATE TABLE `" + name + "` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (AUTO_PARTITIONING_PARTITION_SIZE_MB = 100)"
			runSetup(c, opened, []string{create})
			statement := "ALTER TABLE `" + name + "` " + test.set

			reported := lint.reported(c, []string{create}, statement)
			c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil)

			c.Assert(slices.Contains(reported, "YD118"), qt.Equals, test.reported, qt.Commentf("lint reported %v", reported))
			c.Assert(describedPartitioning(c, opened, name).PartitionSizeMb, qt.Equals, test.size)
		})
	}
}

// familyNames is the names of the column families the table at absolute
// holds, in name order, read back without the provider.
func familyNames(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	names := make([]string, 0)
	for _, family := range heldStorage(c, opened, absolute).Families {
		names = append(names, strings.Fields(family)[0])
	}
	return names
}

// Ported from the host's TestYDBLint_UndeclaredColumnFamilyIsCreated: YDB does
// not refuse an ALTER TABLE that names a column family the table does not
// have; it creates the family with its own settings. YD119 reports each such
// statement, and the server shows what it reports: the family exists after
// the statement. A statement naming a family the table has is the control.
func TestLintUndeclaredColumnFamilyIsCreated(t *testing.T) {
	setup := []string{"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, a Utf8 FAMILY cold, PRIMARY KEY (id), FAMILY cold (COMPRESSION = 'lz4'))"}
	tests := []struct {
		name      string
		statement string
		family    string
		reported  bool
	}{
		{name: "a column moved", statement: "ALTER TABLE `{dir}/t` ALTER COLUMN a SET FAMILY clod", family: "clod", reported: true},
		{name: "a family altered", statement: "ALTER TABLE `{dir}/t` ALTER FAMILY warm SET COMPRESSION 'lz4'", family: "warm", reported: true},
		{name: "a column added", statement: "ALTER TABLE `{dir}/t` ADD COLUMN b Int32 FAMILY hot", family: "hot", reported: true},
		{name: "a family the table has", statement: "ALTER TABLE `{dir}/t` ALTER FAMILY cold SET COMPRESSION 'off'", family: "cold"},
	}
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	lint := newLintSession(c)
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			dir := path.Join(database, directory, fmt.Sprintf("family_%d", i))
			steps := replaced(setup, dir)
			statement := strings.ReplaceAll(test.statement, "{dir}", dir)
			runSetup(c, opened, steps)

			reported := lint.reported(c, steps, statement)
			runErr := execFixtureSchema(c.Context(), opened.DB, statement)

			c.Assert(slices.Contains(reported, "YD119"), qt.Equals, test.reported, qt.Commentf("lint reported %v", reported))
			c.Assert(runErr, qt.IsNil)
			c.Assert(familyNames(c, opened, dir+"/t"), qt.Contains, test.family)
		})
	}
}
