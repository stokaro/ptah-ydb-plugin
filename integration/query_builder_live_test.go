//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The statements here are the texts the host's query builder renders for YDB
// with the query dialect this provider declares: backticked names, a table
// path as one name, and YQL parameters $p1, $p2 and on, bound at the Go type
// the caller passed. The host's core/query/registeredtest/yql_test.go
// registers that dialect and pins the same texts: a change to the builder
// shows there, and a change to what YDB answers shows here. The host sends
// each through a provider as a statement with typed arguments, as these tests
// do.

// int64Argument and the others are one argument of a rendered statement, typed
// as the host types the Go value the builder was given.
func int64Argument(v int64) *protocol.StatementArgument {
	return typedArgument(protocol.ArgumentType_INT64, false, signedValue(v))
}

func int32Argument(v int64) *protocol.StatementArgument {
	return typedArgument(protocol.ArgumentType_INT32, false, signedValue(v))
}

func uint64Argument(v uint64) *protocol.StatementArgument {
	return typedArgument(protocol.ArgumentType_UINT64, false, unsignedValue(v))
}

func float64Argument(v float64) *protocol.StatementArgument {
	return typedArgument(protocol.ArgumentType_FLOAT64, false, doubleValue(v))
}

func stringArgument(v string) *protocol.StatementArgument {
	return typedArgument(protocol.ArgumentType_STRING, false, textValue(v))
}

// dataText is one value of a result row with its kind, so that an assertion
// says whether the provider answered with a signed or an unsigned integer.
func dataText(value *protocol.DataValue) string {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_Null:
		return "Null"
	case *protocol.DataValue_BoolValue:
		return "Bool(" + strconv.FormatBool(v.BoolValue) + ")"
	case *protocol.DataValue_Int64Value:
		return "Int64(" + strconv.FormatInt(v.Int64Value, 10) + ")"
	case *protocol.DataValue_Uint64Value:
		return "Uint64(" + strconv.FormatUint(v.Uint64Value, 10) + ")"
	case *protocol.DataValue_DoubleValue:
		return "Double(" + strconv.FormatFloat(v.DoubleValue, 'g', -1, 64) + ")"
	case *protocol.DataValue_StringValue:
		return "String(" + v.StringValue + ")"
	case *protocol.DataValue_BytesValue:
		return "Bytes(" + strconv.Quote(string(v.BytesValue)) + ")"
	}
	return "unexpected " + value.String()
}

// resultRows is a statement's answer, one line per row and the values in
// order, or the outcome and the error of a statement that did not succeed.
func resultRows(result *protocol.StatementResult) []string {
	if result.GetOutcome() != protocol.StatementResult_SUCCEEDED {
		return []string{result.GetOutcome().String() + ": " + result.GetError()}
	}
	rows := make([]string, len(result.GetRows()))
	for i, row := range result.GetRows() {
		values := make([]string, len(row.GetValues()))
		for j, value := range row.GetValues() {
			values[j] = dataText(value)
		}
		rows[i] = strings.Join(values, " ")
	}
	return rows
}

// Ported from the host's TestYDBQueryBuilder_RunsWhatItRenders, the statement
// half: every kind of statement the host's builder renders for YDB, an
// INSERT, an UPSERT over existing and new rows, a join paged with LIMIT and
// OFFSET, an IN subquery, an uncorrelated EXISTS and a grouped count, runs
// through the provider with the arguments typed as the host types them, and
// each answer is the host's. The UPSERT writes the name of user 1 over the
// stored one, keeps the age it does not name, and inserts user 3. The
// builder itself is the host's, and its yql_test.go pins these texts.
func TestQueryBuilderStatementsRunThroughTheProvider_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	users, orders := path.Join(directory, "users"), path.Join(directory, "orders")
	for _, statement := range []string{
		"CREATE TABLE `" + path.Join(database, users) + "` (id Int64 NOT NULL, name Utf8, age Int32, PRIMARY KEY (id))",
		"CREATE TABLE `" + path.Join(database, orders) + "` (id Int64 NOT NULL, user_id Int64, total Double, PRIMARY KEY (id))",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	client, session := typedSession(c)

	writes := []*protocol.StatementResult{
		runTyped(c, client, session, "INSERT INTO `"+users+"` (`id`, `name`, `age`) VALUES ($p1, $p2, $p3), ($p4, $p5, $p6)",
			int64Argument(1), stringArgument("alice"), int32Argument(30), int64Argument(2), stringArgument("bob"), int32Argument(40)),
		runTyped(c, client, session, "INSERT INTO `"+orders+"` (`id`, `user_id`, `total`) VALUES ($p1, $p2, $p3), ($p4, $p5, $p6), ($p7, $p8, $p9)",
			int64Argument(10), int64Argument(1), float64Argument(5.5), int64Argument(11), int64Argument(1), float64Argument(7),
			int64Argument(12), int64Argument(2), float64Argument(1)),
		runTyped(c, client, session, "UPSERT INTO `"+users+"` (`id`, `name`) VALUES ($p1, $p2), ($p3, $p4)",
			int64Argument(1), stringArgument("alicia"), int64Argument(3), stringArgument("carol")),
	}
	stored := runTyped(c, client, session, "SELECT `id`, `name`, `age` FROM `"+users+"` ORDER BY `id`")
	joined := runTyped(c, client, session, "SELECT `u`.`name`, `o`.`total` FROM `"+users+"` `u` INNER JOIN `"+orders+"` `o` ON `o`.`user_id` = `u`.`id` "+
		"WHERE `o`.`total` > $p1 ORDER BY `o`.`total` DESC LIMIT $p2 OFFSET $p3", float64Argument(2), uint64Argument(1), uint64Argument(1))
	inQuery := runTyped(c, client, session, "SELECT `id` FROM `"+users+"` WHERE `id` IN (SELECT `user_id` FROM `"+orders+"`) ORDER BY `id` ASC")
	exists := runTyped(c, client, session, "SELECT `id` FROM `"+users+"` WHERE EXISTS (SELECT `id` FROM `"+orders+"` WHERE `total` > $p1) ORDER BY `id` ASC",
		float64Argument(6))
	grouped := runTyped(c, client, session, "SELECT `o`.`user_id`, COUNT(*) AS `n` FROM `"+orders+"` `o` GROUP BY `o`.`user_id` HAVING COUNT(*) > $p1",
		uint64Argument(1))

	for _, written := range writes {
		c.Assert(resultRows(written), qt.HasLen, 0)
	}
	c.Assert(resultRows(stored), qt.DeepEquals, []string{
		"Int64(1) String(alicia) Int64(30)", "Int64(2) String(bob) Int64(40)", "Int64(3) String(carol) Null",
	})
	c.Assert(resultRows(joined), qt.DeepEquals, []string{"String(alicia) Double(5.5)"})
	c.Assert(resultRows(inQuery), qt.DeepEquals, []string{"Int64(1)", "Int64(2)"})
	c.Assert(resultRows(exists), qt.DeepEquals, []string{"Int64(1)", "Int64(2)", "Int64(3)"})
	c.Assert(resultRows(grouped), qt.DeepEquals, []string{"Int64(1) Uint64(2)"})
}

// upsertNamingNoIndexColumn is what each certified line answers to an UPSERT
// that names no column a synchronous index of the table is keyed on, on a
// table with a unique index: 26.2.1.14 fails it with an internal error, a
// defect in the server, and 25.1.4.7 runs it. The host's YDB page documents
// the defect and the workaround of naming every column.
var upsertNamingNoIndexColumn = map[string]string{
	"26.2": `(?s).*INTERNAL_ERROR.*verification=!hasUniqIndex \|\| !usedIndexes\.empty\(\);.*fline=kqp_opt_phy_upsert_index\.cpp:359.*`,
	"25.1": `<nil>`,
}

// upsertNamingNoIndexColumnThroughTheProvider is the provider's outcome for
// the same UPSERT. INTERNAL_ERROR does not establish that the server applied
// nothing, so the provider reports the outcome as unknown, and the contract
// carries a server message only with a failure.
var upsertNamingNoIndexColumnThroughTheProvider = map[string]string{"26.2": "UNKNOWN: ", "25.1": "SUCCEEDED: "}

// upsertNamingNoIndexColumnRows is how many rows the table holds after that
// UPSERT on each line: the failed UPSERT wrote nothing.
var upsertNamingNoIndexColumnRows = map[string]int{"26.2": 1, "25.1": 2}

// Ported from the host's TestYDBQueryBuilder_UpsertOnATableWithAUniqueIndex:
// an UPSERT the host's builder renders on a table with a unique index runs on
// both lines through the provider when it names every column. The one that
// leaves out the indexed column gets the answer upsertNamingNoIndexColumn
// records for the line from the server, sent past the provider, and the
// outcome upsertNamingNoIndexColumnThroughTheProvider records through it.
func TestQueryBuilderUpsertOnATableWithAUniqueIndexFollowsTheLine(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	accounts := path.Join(directory, "accounts")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, accounts)+"` (id Int64 NOT NULL, email Utf8, plan Utf8, "+
		"PRIMARY KEY (id), INDEX accounts_email GLOBAL UNIQUE SYNC ON (email))"), qt.IsNil)
	client := startProviderFor(c, provider.TypedArgumentsFeature)
	opening, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), opening.Id), qt.IsNil) })
	session, line := opening.Id, serverLine(c, opening.ServerVersion)
	upsert := "UPSERT INTO `" + accounts + "` (`id`, `email`, `plan`) VALUES ($p1, $p2, $p3)"
	read := "SELECT `id`, `email`, `plan` FROM `" + accounts + "` ORDER BY `id`"

	first := runTyped(c, client, session, upsert, int64Argument(1), stringArgument("a@x"), stringArgument("free"))
	partial := runTyped(c, client, session, "UPSERT INTO `"+accounts+"` (`id`, `plan`) VALUES ($p1, $p2)", int64Argument(2), stringArgument("trial"))
	// #nosec G202 -- the path is a table under a directory this test generated
	_, serverErr := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+path.Join(database, accounts)+"` (`id`, `plan`) VALUES (2l, 'trial'u)")
	afterPartial := runTyped(c, client, session, read)
	paid := runTyped(c, client, session, upsert, int64Argument(1), stringArgument("a@x"), stringArgument("paid"))
	afterPaid := runTyped(c, client, session, read)

	c.Assert(resultRows(first), qt.HasLen, 0)
	c.Assert(fmt.Sprint(serverErr), qt.Matches, upsertNamingNoIndexColumn[line])
	c.Assert(partial.GetOutcome().String()+": "+partial.GetError(), qt.Equals, upsertNamingNoIndexColumnThroughTheProvider[line])
	c.Assert(resultRows(afterPartial), qt.HasLen, upsertNamingNoIndexColumnRows[line])
	c.Assert(resultRows(paid), qt.HasLen, 0)
	c.Assert(resultRows(afterPaid)[0], qt.Equals, "Int64(1) String(a@x) String(paid)")
}
