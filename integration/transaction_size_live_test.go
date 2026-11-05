//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// copyLimitAnswers is how each line refuses a copy larger than one query may
// carry: 26.2 runs out of its 64 MiB write buffer while the query runs, and
// 25.1 exceeds the 48 MiB datashard program at the commit and answers
// ABORTED, the status a lock conflict carries too.
var copyLimitAnswers = map[string]string{
	"26.2": `(?s).*Out of buffer memory\. Used \d+ bytes of 67108864 bytes.*`,
	"25.1": `(?s).*Datashard program size limit exceeded \(\d+ > 50331648\).*`,
}

// largeTable creates the table at absolute with rows rows of about 80 bytes
// each, generated on the server a batch per query, and drops it when the test
// ends.
func largeTable(c *qt.C, opened *connection.Connection, absolute string, rows int) {
	c.Helper()
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, v Utf8, w Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	const batch = 20000
	for start := 0; start < rows; start += batch {
		_, err := opened.DB.ExecContext(c.Context(), fmt.Sprintf("UPSERT INTO `%s` (id, v, w) SELECT CAST(x AS Int64) AS id, "+
			"CAST(x AS Utf8) || 'value-padding-padding-padding'u AS v, 'more-padding-more-padding-more-padding'u AS w "+
			"FROM AS_TABLE(ListMap(ListFromRange(%d, %d), ($x) -> (AsStruct($x AS x))))", absolute, start, min(start+batch, rows)))
		c.Assert(err, qt.IsNil)
	}
}

// Ported from the host's TestYDBRebuild_ACopyTooLargeForOneQueryIsNotRetried,
// at the provider: a rebuild copies a table in one query, and a copy of
// 600000 rows of about 80 bytes in a caller transaction is more than one query
// may carry on both lines, so nothing is copied. Neither the unit nor the
// commit says YDB aborted the transaction for a conflict, so a host that runs
// such a transaction again does not run this one again: on 25.1 the commit
// answers ABORTED with issue 200509, which every new attempt answers too.
func TestTransactionTooLargeForOneQueryIsNotAborted_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	source, target := path.Join(database, directory, "big"), path.Join(database, directory, "big_copy")
	largeTable(c, opened, source, 600000)
	largeTable(c, opened, target, 0)
	client, session := splitSession(c, provider.MigrationSplitFeature)
	beginSerializable(c, client, session, "copy")

	// The unit's own error differs by line: 26.2 refuses the unit, and 25.1
	// runs it and refuses the commit. The messages say which.
	inside, _ := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session, RequestId: "copy", TransactionId: "copy",
		Units: []*protocol.Unit{{ChangeId: "copy", Body: &protocol.Unit_Sql{Sql: "INSERT INTO `" + target + "` (id, v, w) SELECT id, v, w FROM `" + source + "`"}}}})
	finished, finishErr := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session, TransactionId: "copy",
		Action: protocol.FinishTransactionRequest_COMMIT})

	c.Assert(inside.GetTransactionAborted(), qt.IsFalse)
	c.Assert(finishErr, qt.ErrorIs, provider.ErrExecution)
	c.Assert(finished.GetOutcome(), qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(finished.GetAborted(), qt.IsFalse)
	c.Assert(inside.GetMessage()+"\n"+finished.GetMessage(), qt.Matches, copyLimitAnswers[serverLine(c, serverVersion(c, opened))])
	assertRowCount(c, opened, target, 0)
}

// copyLimitReasons is what the host prints of each line's refusal of a copy
// too large for one query, through this provider: the outcome and the
// provider's reason, which names the status and the issue code YDB answered
// with.
var copyLimitReasons = map[string]string{
	"26.2": "provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement (PRECONDITION_FAILED, issue 2029)",
	"25.1": "provider transaction outcome is ROLLED_BACK: YDB refused the transaction at its commit: its writes do not fit one datashard program, " +
		"which holds about 48 MiB; write the rows in smaller transactions (ABORTED, issue 200509)",
}

// Ported from the host's TestYDBRebuild_ACopyTooLargeForOneQueryIsNotRetried,
// through `migrations up`: the host runs a data query and its checkpoint in
// one transaction, and runs the transaction again when the provider says YDB
// aborted it for a conflict. A copy too large for one query fails once with
// the line's refusal, not with an abort for a conflict after the host ran the
// copy again, and nothing is copied.
func TestHostRefusesACopyTooLargeForOneQuery_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	c.Assert(f.opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(f.database, f.dir)), qt.IsNil)
	largeTable(c, f.opened, path.Join(f.database, f.table("big")), 600000)
	largeTable(c, f.opened, path.Join(f.database, f.table("big_copy")), 0)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_copy.up.sql":   "INSERT INTO `" + f.table("big_copy") + "` (id, v, w) SELECT id, v, w FROM `" + f.table("big") + "`;\n",
		"0000000001_copy.down.sql": "DELETE FROM `" + f.table("big_copy") + "`;\n",
	})

	failure, err := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", failure))
	c.Assert(failure, qt.Contains, copyLimitReasons[serverLine(c, serverVersion(c, f.opened))])
	c.Assert(failure, qt.Not(qt.Contains), "aborted the transaction for a conflict")
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", failed, 0, 1}})
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("big_copy")+"`"), qt.Equals, int64(0))
}
