//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// serialTables are the tables the Serial fixtures declare, each keyed by a
// Serial id beside a nullable note.
var serialTables = []string{"orders", "counters", "plain"}

// hostSerials declares the Serial fixtures in directory, as Go annotations: a
// BIGSERIAL orders that starts at 100 and steps by ordersIncrement, a BIGINT
// auto_increment counters that steps by countersIncrement from 1, and a SERIAL
// plain whose further attributes are plain.
func hostSerials(directory, ordersIncrement, countersIncrement, plain string) string {
	table := func(name, structName, id string) string {
		return "//ptah:schema:table name=\"" + name + "\" schema=\"" + directory + "\"\ntype " + structName + " struct {\n" +
			"\t//ptah:schema:field name=\"id\" " + id + "\n\tID int64\n\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n}\n\n"
	}
	return "package entities\n\n" +
		table("orders", "Order", `type="BIGSERIAL" primary="true" identity_start="100" identity_increment="`+ordersIncrement+`"`) +
		table("counters", "Counter", `type="BIGINT" primary="true" auto_increment="true" identity_increment="`+countersIncrement+`"`) +
		table("plain", "Plain", `type="SERIAL" primary="true"`+plain)
}

// serialFixture is the lock fixture with the Serial tables dropped when the
// test ends.
func serialFixture(c *qt.C) (opened *connection.Connection, directory, database string) {
	c.Helper()
	opened, directory, database = lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range serialTables {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	return opened, directory, database
}

// serialIDs inserts one row per note into table, naming no id, and returns
// every id the table holds in order.
func serialIDs(c *qt.C, opened *connection.Connection, absolute string, notes ...string) []int64 {
	c.Helper()
	for _, note := range notes {
		_, err := opened.DB.ExecContext(c.Context(), "INSERT INTO `"+absolute+"` (note) VALUES ('"+note+"'u)") // #nosec G202 -- the path is a table under a directory this test generated, and note is a letter its callers pass
		c.Assert(err, qt.IsNil)
	}
	rows, err := opened.DB.QueryContext(c.Context(), "SELECT CAST(id AS Int64) FROM `"+absolute+"` ORDER BY id") // #nosec G202 -- the path is a table under a directory this test generated
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	var ids []int64
	for rows.Next() {
		var id int64
		c.Assert(rows.Scan(&id), qt.IsNil)
		ids = append(ids, id)
	}
	c.Assert(rows.Err(), qt.IsNil)
	return ids
}

// heldSequence is what the server holds of the sequence of a table's id: its
// start, its increment, the value of the last restart (0 for none) and its
// maximum.
type heldSequence struct {
	Start, Increment, Restart, Maximum int64
}

func describedSequence(c *qt.C, opened *connection.Connection, absolute string) heldSequence {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	sequence := described.GetColumns()[0].GetFromSequence()
	c.Assert(sequence, qt.IsNotNil)
	return heldSequence{Start: sequence.GetStartValue(), Increment: sequence.GetIncrement(), Restart: sequence.GetSetVal().GetNextValue(), Maximum: sequence.GetMaxValue()}
}

// The host creates Serial keys with a start and an increment through the
// provider, finds them as declared on the next run, and the rows the tables
// then take come from the declared sequences. The native call gives a new
// sequence its start without a restart, so the server records none. Changing
// an increment on a table that holds rows is one sequence change without a
// restart, and the next row follows on from the last one.
func TestHostSerialSequences_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	table := func(name string) string { return path.Join(database, directory, name) }
	for _, run := range []struct{ name, source, want string }{
		{"creation", hostSerials(directory, "5", "3", ""), `(?s).*row-table-create-operation.*Schema apply completed successfully.*`},
		{"a second run", hostSerials(directory, "5", "3", ""), `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(describedSequence(c, opened, table("orders")), qt.Equals, heldSequence{Start: 100, Increment: 5, Maximum: 9223372036854775807})
	c.Assert(describedSequence(c, opened, table("counters")), qt.Equals, heldSequence{Start: 1, Increment: 3, Maximum: 9223372036854775807})
	c.Assert(describedSequence(c, opened, table("plain")), qt.Equals, heldSequence{Start: 1, Increment: 1, Maximum: 2147483647})
	c.Assert(serialIDs(c, opened, table("orders"), "a", "b"), qt.DeepEquals, []int64{100, 105})
	c.Assert(serialIDs(c, opened, table("counters"), "a", "b"), qt.DeepEquals, []int64{1, 4})
	c.Assert(serialIDs(c, opened, table("plain"), "a", "b"), qt.DeepEquals, []int64{1, 2})
	for _, run := range []struct{ name, want string }{
		{"another increment", `(?s).*serial-sequence-operation.*Schema apply completed successfully.*`},
		{"a run after the change", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, hostSerials(directory, "5", "7", ""))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(describedSequence(c, opened, table("counters")), qt.Equals, heldSequence{Start: 1, Increment: 7, Maximum: 9223372036854775807})
	// The sequence had computed 7 as its next value before the change, and
	// steps by 7 from there.
	c.Assert(serialIDs(c, opened, table("counters"), "c", "d"), qt.DeepEquals, []int64{1, 4, 7, 14})
}

// What the server would break is refused while planning, and changing it by
// hand shows the server does what the refusal says. A sequence some RESTART
// moved has that restart replayed by every later ALTER SEQUENCE, so the next
// insert takes a key a row holds. A Serial's ALTER SEQUENCE raises its maximum
// to the Int64 maximum, so the value after the column's maximum is stored as
// a negative number.
func TestHostSerialSequences_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	table := func(name string) string { return path.Join(database, directory, name) }
	sequence := func(name string) string { return "`" + table(name) + "/_serial_column_id`" }
	output, err := hostApply(c, directory, hostSerials(directory, "5", "3", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(serialIDs(c, opened, table("orders"), "a", "b"), qt.DeepEquals, []int64{100, 105})
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER SEQUENCE "+sequence("orders")+" RESTART WITH 110"), qt.IsNil)
	c.Assert(serialIDs(c, opened, table("orders"), "c"), qt.DeepEquals, []int64{100, 105, 110})

	for _, test := range []struct{ name, source, want string }{
		{"a restarted sequence", hostSerials(directory, "10", "3", ""),
			fmt.Sprintf(`(?s).*the sequence of column "id" of table "%s/orders": the sequence was restarted at 110, and YDB replays that restart on every later ALTER SEQUENCE.*`, directory)},
		{"a start on a 32-bit Serial", hostSerials(directory, "5", "3", ` identity_start="100"`),
			`(?s).*giving the sequence of column "id" of table "` + directory + `/plain" a start or an increment \(an ALTER SEQUENCE raises the maximum of a Serial's sequence ` +
				`from 2147483647 to the Int64 maximum, .*\), which requires target capability serial_sequence_keeps_range, unavailable on this ydb target.*`},
	} {
		output, err := hostApply(c, directory, test.source)
		c.Assert(err, qt.IsNotNil, qt.Commentf("%s: %s", test.name, output))
		c.Assert(output, qt.Matches, test.want, qt.Commentf("%s", test.name))
	}

	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER SEQUENCE "+sequence("orders")+" INCREMENT BY 5"), qt.IsNil)
	_, err = opened.DB.ExecContext(c.Context(), "INSERT INTO `"+table("orders")+"` (note) VALUES ('d'u)") // #nosec G202 -- the path is a table under a directory this test generated
	c.Assert(err, qt.ErrorMatches, `(?s).*Conflict with existing key.*`)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER SEQUENCE "+sequence("plain")+" RESTART WITH 2147483647"), qt.IsNil)
	c.Assert(serialIDs(c, opened, table("plain"), "a", "b"), qt.DeepEquals, []int64{-2147483648, 2147483647})
}

// A 32-bit Serial declared with a start is refused before its table is
// created, as the host's embedded engine refuses it: the ALTER SEQUENCE a
// migration file would give it raises its maximum.
func TestHostSerialCreation_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	output, err := hostApply(c, directory, hostSerials(directory, "5", "3", ` identity_start="100"`))
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*giving the sequence of column "id" of table "`+directory+`/plain" a start or an increment .*serial_sequence_keeps_range.*`)
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}

// serialYQL is the Serial fixtures as a YQL schema, in the database at
// database: ALTER SEQUENCE names a sequence by its absolute path. orders
// starts at 100 and steps by 5, counters steps by countersIncrement.
func serialYQL(database, directory string, countersIncrement int) string {
	sequence := func(name string) string { return "`" + path.Join(database, directory, name) + "/_serial_column_id`" }
	return fmt.Sprintf("CREATE TABLE `%[1]s/orders` (id BigSerial NOT NULL, note Utf8, PRIMARY KEY (id));\n"+
		"ALTER SEQUENCE %[2]s START WITH 100 INCREMENT BY 5;\n"+
		"CREATE TABLE `%[1]s/counters` (id BigSerial NOT NULL, note Utf8, PRIMARY KEY (id));\n"+
		"ALTER SEQUENCE %[3]s INCREMENT %[4]d;\n", directory, sequence("orders"), sequence("counters"), countersIncrement)
}

// A YQL desired schema states a Serial's start and increment with ALTER
// SEQUENCE. The host applies it through the provider, plans nothing on the
// next run, the rows follow the declared sequences, and a changed increment
// is one sequence change.
func TestHostSerialSequencesFromYQL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	table := func(name string) string { return path.Join(database, directory, name) }
	for _, run := range []struct {
		name      string
		increment int
		want      string
	}{
		{"creation", 3, `(?s).*row-table-create-operation.*Schema apply completed successfully.*`},
		{"a second run", 3, `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySQL(c, directory, writeSchema(c, map[string]string{"schema.sql": serialYQL(database, directory, run.increment)}))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(serialIDs(c, opened, table("orders"), "a", "b"), qt.DeepEquals, []int64{100, 105})
	c.Assert(serialIDs(c, opened, table("counters"), "a", "b"), qt.DeepEquals, []int64{1, 4})
	for _, run := range []struct{ name, want string }{
		{"another increment", `(?s).*serial-sequence-operation.*Schema apply completed successfully.*`},
		{"a run after the change", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySQL(c, directory, writeSchema(c, map[string]string{"schema.sql": serialYQL(database, directory, 7)}))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(serialIDs(c, opened, table("counters"), "c", "d"), qt.DeepEquals, []int64{1, 4, 7, 14})
}

// hostAccounts declares, in directory, accounts keyed by a BIGINT
// auto_increment id with a NOT NULL email that defaults to 'none', and
// memberships keyed by (account, team).
func hostAccounts(directory string) string {
	return "package entities\n\n//ptah:schema:table name=\"accounts\" schema=\"" + directory + "\"\ntype Account struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" auto_increment=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"email\" type=\"Utf8\" not_null=\"true\" default=\"none\"\n\tEmail string\n}\n\n" +
		"//ptah:schema:table name=\"memberships\" schema=\"" + directory + "\" primary_key=\"account,team\"\ntype Membership struct {\n" +
		"\t//ptah:schema:field name=\"account\" type=\"Int64\" primary=\"true\"\n\tAccount int64\n" +
		"\t//ptah:schema:field name=\"team\" type=\"Utf8\" primary=\"true\"\n\tTeam string\n" +
		"\t//ptah:schema:field name=\"role\" type=\"Utf8\"\n\tRole string\n}\n"
}

// A Go schema with a Serial key, a literal default and a composite key
// applies through the provider, and the host's comparison then finds the
// database as declared. The server holds the key as a Serial and the default
// on the email.
func TestHostSerialKeyComparesClean_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"accounts", "memberships"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	output, err := hostApply(c, directory, hostAccounts(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = hostCompare(c, directory, hostAccounts(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Not(qt.Matches), `(?s).*(CREATE|ALTER|DROP) .*`)
	accounts := path.Join(database, directory, "accounts")
	c.Assert(describedSequence(c, opened, accounts), qt.Equals, heldSequence{Start: 1, Increment: 1, Maximum: 9223372036854775807})
	_, err = opened.DB.ExecContext(c.Context(), "INSERT INTO `"+accounts+"` (email) VALUES ('a@example.com'u)") // #nosec G202 -- the path is a table under a directory this test generated
	c.Assert(err, qt.IsNil)
	_, err = opened.DB.ExecContext(c.Context(), "INSERT INTO `"+accounts+"` (id) VALUES (100)") // #nosec G202 -- the path is a table under a directory this test generated
	c.Assert(err, qt.IsNil)
	var id int64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT id FROM `"+accounts+"` WHERE email = 'a@example.com'u").Scan(&id), qt.IsNil)
	c.Assert(id, qt.Equals, int64(1))
	var email string
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT email FROM `"+accounts+"` WHERE id = 100").Scan(&email), qt.IsNil)
	c.Assert(email, qt.Equals, "none")
}

// hostCompare runs the host's `schema compare --exit-code` over one Go source
// with this provider selected for ydb, restricted to the fixture directory,
// and returns what the host printed and how it exited: 1 when the database
// differs from the source.
func hostCompare(c *qt.C, directory, source string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(root, "entities.go"), []byte(source), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "compare", "--exit-code", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", root, "--schemas", directory)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// hostApplyRebuild is hostApply with the native surface's request for a table
// rebuild.
func hostApplyRebuild(c *qt.C, directory, source string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(root, "entities.go"), []byte(source), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--allow-table-rebuild", "--provider-config", manifest,
		"--provider-engine", "ydb", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", root, "--schemas", directory)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// A change of a table with a Serial key that only a rebuild makes is refused
// while planning even when the operator asks for a rebuild: the new table's
// sequence would start at 1 under the copied rows. The table keeps its rows.
func TestHostSerialRebuild_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := serialFixture(c)
	output, err := hostApply(c, directory, hostSerials(directory, "5", "3", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	orders := path.Join(database, directory, "orders")
	c.Assert(serialIDs(c, opened, orders, "a"), qt.DeepEquals, []int64{100})
	retyped := strings.Replace(hostSerials(directory, "5", "3", ""), `name="note" type="Utf8"`, `name="note" type="Int64"`, 1)
	output, err = hostApplyRebuild(c, directory, retyped)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*rebuilding table "`+directory+`/orders": column "id" takes its values from a sequence\. The new table's sequence would start at 1 `+
		`while the copied rows keep theirs, so the next insert would collide.*`)
	c.Assert(serialIDs(c, opened, orders), qt.DeepEquals, []int64{100})
}
