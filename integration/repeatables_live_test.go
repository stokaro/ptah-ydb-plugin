//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"sync"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/repeatables"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// repeatableTable is the record's name beside a revision table called
// revisions, as the host names it.
const repeatableTable = "revisions_repeatable"

// repeatableClient is a provider process of its own, for a host that lists
// the record, and a session on the test database. scope selects the record
// in directory through that session.
type repeatableClient struct {
	client *providerruntime.Client
	scope  *protocol.RepeatableScope
}

func newRepeatableClient(c *qt.C, binary, digest, directory string) repeatableClient {
	c.Helper()
	client := startProviderListing(c, binary, digest, append(hostFeatures(), provider.RepeatableRecordFeature))
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return repeatableClient{client: client, scope: &protocol.RepeatableScope{SessionId: session.Id, Schema: directory, Table: repeatableTable}}
}

// dropRecordAtCleanup drops the record in directory when the test ends, if a
// call created it.
func dropRecordAtCleanup(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	c.Cleanup(func() {
		// The test failed before the record existed when the drop finds
		// nothing, and the directory's own cleanup reports what is left.
		_, _ = opened.DB.ExecContext(context.Background(), "DROP TABLE `"+path.Join(database, directory, repeatableTable)+"`")
	})
}

func (r repeatableClient) prepare(c *qt.C) *protocol.RepeatableResult {
	c.Helper()
	result, err := r.client.PrepareRepeatables(c.Context(), &protocol.PrepareRepeatablesRequest{Scope: r.scope, RequestId: "prepare"})
	c.Assert(result, qt.IsNotNil)
	c.Assert(err == nil, qt.Equals, result.GetOutcome() == protocol.RepeatableResult_APPLIED, qt.Commentf("%v", err))
	return result
}

func (r repeatableClient) read(c *qt.C) *protocol.RepeatableSnapshot {
	c.Helper()
	result, err := r.client.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{Scope: r.scope, MaxRows: provider.MaxRepeatableRows})
	c.Assert(err, qt.IsNil)
	return result
}

func (r repeatableClient) body(c *qt.C, identity string) *protocol.RepeatableBody {
	c.Helper()
	result, err := r.client.ReadRepeatableBody(c.Context(), &protocol.ReadRepeatableBodyRequest{Scope: r.scope, Identity: identity})
	c.Assert(err, qt.IsNil)
	return result
}

// write sends one write and returns its receipt. Only APPLIED comes back
// without an error, and every other outcome carries its receipt too.
func (r repeatableClient) write(c *qt.C, request *protocol.MutateRepeatableRequest) *protocol.RepeatableResult {
	c.Helper()
	result, err := r.client.MutateRepeatable(c.Context(), request)
	c.Assert(result, qt.IsNotNil, qt.Commentf("%v", err))
	c.Assert(err == nil, qt.Equals, result.GetOutcome() == protocol.RepeatableResult_APPLIED, qt.Commentf("%v", err))
	return result
}

func receipt(result *protocol.RepeatableResult) string {
	return fmt.Sprintf("%s holder=%q removed=%t", result.GetOutcome(), result.GetHolderRunId(), result.GetRemoved())
}

func claim(scope *protocol.RepeatableScope, identity, run, expected, digest string, order uint32) *protocol.MutateRepeatableRequest {
	return &protocol.MutateRepeatableRequest{Scope: proto.CloneOf(scope), RequestId: "claim-" + run, Mutation: &protocol.MutateRepeatableRequest_Claim{
		Claim: &protocol.RepeatableClaim{Identity: identity, Insert: expected == "", ExpectedRunId: expected, RunId: run, AttemptChecksum: digest,
			AttemptOrder: order, Transactional: true, AttemptedAtSeconds: 1795000000, AttemptedAtNanos: 123456789}}}
}

func complete(scope *protocol.RepeatableScope, identity, run, digest string, body []byte) *protocol.MutateRepeatableRequest {
	return &protocol.MutateRepeatableRequest{Scope: proto.CloneOf(scope), RequestId: "complete-" + run, Mutation: &protocol.MutateRepeatableRequest_Complete{
		Complete: &protocol.RepeatableCompletion{Identity: identity, RunId: run, Checksum: digest, Body: body, AppliedAtSeconds: 1795000001, AppliedAtNanos: 7}}}
}

func fail(scope *protocol.RepeatableScope, identity, run, message string) *protocol.MutateRepeatableRequest {
	return &protocol.MutateRepeatableRequest{Scope: proto.CloneOf(scope), RequestId: "fail-" + run, Mutation: &protocol.MutateRepeatableRequest_Fail{
		Fail: &protocol.RepeatableFailure{Identity: identity, RunId: run, Error: message}}}
}

func remove(scope *protocol.RepeatableScope, identity string) *protocol.MutateRepeatableRequest {
	return &protocol.MutateRepeatableRequest{Scope: proto.CloneOf(scope), RequestId: "remove-" + identity, Mutation: &protocol.MutateRepeatableRequest_Remove{
		Remove: &protocol.RepeatableRemoval{Identity: identity}}}
}

// rowText is the part of a row a run decides, in one line.
func rowText(row *protocol.RepeatableRow) string {
	return fmt.Sprintf("%s %s run=%s attempt=%s/%d applied=%t:%s body=%t error=%q", row.GetIdentity(), row.GetState(), row.GetRunId(),
		row.GetAttemptChecksum(), row.GetAttemptOrder(), row.GetApplied(), row.GetChecksum(), row.GetBodyKept(), row.GetError())
}

func rowTexts(snapshot *protocol.RepeatableSnapshot) []string {
	texts := make([]string, 0, len(snapshot.GetRows()))
	for _, row := range snapshot.GetRows() {
		texts = append(texts, rowText(row))
	}
	return texts
}

// The record goes through every write a run makes, and a new provider
// process reads what the last one wrote: a claim, a CONFLICT naming the
// holder, a failure, a takeover of the failed run, a completion of another
// digest refused and of the claimed one applied with its body, and a removal
// that reports whether it deleted a row.
func TestRepeatableRecordSubprocess_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	dropRecordAtCleanup(c, opened, database, directory)
	binary, digest := providerBinary(c)
	first := newRepeatableClient(c, binary, digest, directory)
	scope := first.scope
	body := []byte("UPSERT INTO `names` (id) VALUES (1);\r\n\x00π")

	missing := first.read(c)
	absentBody := first.body(c, "names")
	prepared := first.prepare(c)
	again := first.prepare(c)
	empty := first.read(c)
	claimed := first.write(c, claim(scope, "names", "run-1", "", "digest-1", 1))
	contested := first.write(c, claim(scope, "names", "run-2", "", "digest-1", 1))
	failed := first.write(c, fail(scope, "names", "run-1", "line one\nline two"))
	afterFailure := first.read(c)
	notKept := first.body(c, "names")
	stale := first.write(c, claim(scope, "names", "run-2", "run-0", "digest-2", 2))
	takenOver := first.write(c, claim(scope, "names", "run-2", "run-1", "digest-2", 2))
	wrongDigest := first.write(c, complete(scope, "names", "run-2", "digest-1", body))
	otherRun := first.write(c, complete(scope, "names", "run-1", "digest-2", body))
	completed := first.write(c, complete(scope, "names", "run-2", "digest-2", body))

	second := newRepeatableClient(c, binary, digest, directory)
	durable := second.read(c)
	kept := second.body(c, "names")
	removed := second.write(c, remove(second.scope, "names"))
	removedAgain := second.write(c, remove(second.scope, "names"))
	afterRemoval := second.read(c)

	c.Assert(missing.GetExists(), qt.IsFalse)
	c.Assert(missing.GetRows(), qt.HasLen, 0)
	c.Assert(absentBody.GetFound(), qt.IsFalse)
	c.Assert(receipt(prepared), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(receipt(again), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(empty.GetExists(), qt.IsTrue)
	c.Assert(empty.GetRows(), qt.HasLen, 0)
	c.Assert(receipt(claimed), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(receipt(contested), qt.Equals, `CONFLICT holder="run-1" removed=false`)
	c.Assert(receipt(failed), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(rowTexts(afterFailure), qt.DeepEquals, []string{`names FAILED run=run-1 attempt=digest-1/1 applied=false: body=false error="line one\nline two"`})
	c.Assert(notKept.GetFound(), qt.IsTrue)
	c.Assert(notKept.GetKept(), qt.IsFalse)
	c.Assert(receipt(stale), qt.Equals, `CONFLICT holder="run-1" removed=false`)
	c.Assert(receipt(takenOver), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(receipt(wrongDigest), qt.Equals, `REFUSED holder="" removed=false`)
	c.Assert(receipt(otherRun), qt.Equals, `CONFLICT holder="run-2" removed=false`)
	c.Assert(receipt(completed), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(rowTexts(durable), qt.DeepEquals, []string{`names APPLIED run=run-2 attempt=digest-2/2 applied=true:digest-2 body=true error=""`})
	c.Assert(durable.GetRows()[0].GetAttemptedAtSeconds(), qt.Equals, int64(1795000000))
	c.Assert(durable.GetRows()[0].GetAppliedAtNanos(), qt.Equals, int32(7))
	c.Assert(kept.GetKept(), qt.IsTrue)
	c.Assert(kept.GetBody(), qt.DeepEquals, body)
	c.Assert(receipt(removed), qt.Equals, `APPLIED holder="" removed=true`)
	c.Assert(receipt(removedAgain), qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(afterRemoval.GetRows(), qt.HasLen, 0)
}

// race sends one write from every client at once and returns each receipt
// under the run the write claims for.
func race(clients []repeatableClient, request func(int, repeatableClient) *protocol.MutateRepeatableRequest) map[string]string {
	var mu sync.Mutex
	var group sync.WaitGroup
	start := make(chan struct{})
	receipts := make(map[string]string, len(clients))
	for i, client := range clients {
		group.Go(func() {
			sent := request(i, client)
			<-start
			result, err := client.client.MutateRepeatable(context.Background(), sent)
			mu.Lock()
			defer mu.Unlock()
			receipts[sent.GetClaim().GetRunId()] = fmt.Sprintf("%s %v", receipt(result), err != nil)
		})
	}
	close(start)
	group.Wait()
	return receipts
}

// expectedRace is the receipt of every run in a race that winner won: the
// winner's claim applied and every other one met a CONFLICT naming it.
func expectedRace(runs []string, winner string) map[string]string {
	expected := make(map[string]string, len(runs))
	for _, run := range runs {
		expected[run] = fmt.Sprintf(`CONFLICT holder=%q removed=false true`, winner)
	}
	expected[winner] = `APPLIED holder="" removed=false false`
	return expected
}

// Claims sent at once from separate provider processes settle as one: one
// applies, and every other reads the winner's row and meets a CONFLICT that
// names it, on a new identity and on a takeover of the winner's run alike.
func TestRepeatableRecordConcurrentClaims_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	dropRecordAtCleanup(c, opened, database, directory)
	binary, digest := providerBinary(c)
	clients := make([]repeatableClient, 4)
	inserting, takingOver := make([]string, len(clients)), make([]string, len(clients))
	for i := range clients {
		clients[i] = newRepeatableClient(c, binary, digest, directory)
		inserting[i], takingOver[i] = fmt.Sprintf("insert-%d", i), fmt.Sprintf("take-%d", i)
	}
	clients[0].prepare(c)

	inserted := race(clients, func(i int, client repeatableClient) *protocol.MutateRepeatableRequest {
		return claim(client.scope, "names", inserting[i], "", "digest-1", 1)
	})
	winner := clients[0].read(c).GetRows()[0].GetRunId()
	takenOver := race(clients, func(i int, client repeatableClient) *protocol.MutateRepeatableRequest {
		return claim(client.scope, "names", takingOver[i], winner, "digest-2", 2)
	})
	holder := clients[0].read(c).GetRows()[0].GetRunId()

	c.Assert(inserting, qt.Contains, winner)
	c.Assert(inserted, qt.DeepEquals, expectedRace(inserting, winner))
	c.Assert(takingOver, qt.Contains, holder)
	c.Assert(takenOver, qt.DeepEquals, expectedRace(takingOver, holder))
	c.Assert(rowTexts(clients[1].read(c)), qt.DeepEquals, []string{fmt.Sprintf(`names RUNNING run=%s attempt=digest-2/2 applied=false: body=false error=""`, holder)})
}

// A completion that carries a transaction writes in it beside the
// migration's own statements: another session does not see it before the
// commit, a rollback leaves the claim and no body, and a commit applies the
// row and the statements together.
func TestRepeatableRecordCompletionFollowsTheTransaction_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	dropRecordAtCleanup(c, opened, database, directory)
	application := path.Join(database, directory, "application")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+application+"` (id Uint64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+application+"`")
		c.Check(err, qt.IsNil)
	})
	binary, digest := providerBinary(c)
	runner := newRepeatableClient(c, binary, digest, directory)
	observer := newRepeatableClient(c, binary, digest, directory)
	runner.prepare(c)
	runner.write(c, claim(runner.scope, "names", "run-1", "", "digest-1", 1))
	attempt := func(id string, action protocol.FinishTransactionRequest_Action) (string, string, *protocol.TransactionResult) {
		tx, err := runner.client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: runner.scope.SessionId, TransactionId: id,
			Isolation: protocol.TransactionIsolation_SERIALIZABLE})
		c.Assert(err, qt.IsNil)
		bound := proto.CloneOf(runner.scope)
		bound.TransactionId = tx.Id
		c.Assert(executeSQL(c, runner.client, runner.scope.SessionId, tx.Id, "UPSERT INTO `"+application+"` (id) VALUES (1u)").Outcome, qt.Equals, protocol.Execution_APPLIED)
		written := receipt(runner.write(c, complete(bound, "names", "run-1", "digest-1", []byte("body"))))
		during := rowTexts(observer.read(c))[0]
		finished, err := runner.client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: runner.scope.SessionId, TransactionId: tx.Id, Action: action})
		c.Assert(err, qt.IsNil)
		return written, during, finished
	}

	rolledBackWrite, rolledBackDuring, rolledBack := attempt("rollback", protocol.FinishTransactionRequest_ROLLBACK)
	afterRollback := rowTexts(observer.read(c))
	noBody := observer.body(c, "names")
	assertRowCount(c, opened, application, 0)
	committedWrite, committedDuring, committed := attempt("commit", protocol.FinishTransactionRequest_COMMIT)
	afterCommit := rowTexts(observer.read(c))
	kept := observer.body(c, "names")

	running := `names RUNNING run=run-1 attempt=digest-1/1 applied=false: body=false error=""`
	c.Assert(rolledBackWrite, qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(rolledBackDuring, qt.Equals, running)
	c.Assert(rolledBack.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(afterRollback, qt.DeepEquals, []string{running})
	c.Assert(noBody.GetKept(), qt.IsFalse)
	c.Assert(committedWrite, qt.Equals, `APPLIED holder="" removed=false`)
	c.Assert(committedDuring, qt.Equals, running)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	c.Assert(afterCommit, qt.DeepEquals, []string{`names APPLIED run=run-1 attempt=digest-1/1 applied=true:digest-1 body=true error=""`})
	c.Assert(kept.GetBody(), qt.DeepEquals, []byte("body"))
	assertRowCount(c, opened, application, 1)
}

// A table of the record's name that the provider did not create, or whose
// layout changed after it did, is refused by every call, and its rows are
// left as they were.
func TestRepeatableRecordRefusesForeignAndChangedStorage_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	binary, digest := providerBinary(c)
	t.Run("an unmarked table", func(t *testing.T) {
		c := testcheck.New(t)
		opened, directory, database := inspectionFixture(c)
		dropRecordAtCleanup(c, opened, database, directory)
		table := path.Join(database, directory, repeatableTable)
		_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (identity Utf8 NOT NULL, row String NOT NULL, body String NOT NULL, PRIMARY KEY (identity))")
		c.Assert(err, qt.IsNil)
		_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+table+"` (identity, row, body) VALUES ('names', 'kept', '')") // #nosec G202 -- the path is a table under a directory this test generated
		c.Assert(err, qt.IsNil)
		client := newRepeatableClient(c, binary, digest, directory)

		prepared := client.prepare(c)
		_, readErr := client.client.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{Scope: client.scope, MaxRows: 10})
		_, bodyErr := client.client.ReadRepeatableBody(c.Context(), &protocol.ReadRepeatableBodyRequest{Scope: client.scope, Identity: "names"})
		claimed := client.write(c, claim(client.scope, "names", "run-1", "", "digest-1", 1))
		removed := client.write(c, remove(client.scope, "names"))
		var row string
		c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT CAST(row AS Utf8) FROM `"+table+"` WHERE identity = 'names'").Scan(&row), qt.IsNil)

		c.Assert(receipt(prepared), qt.Equals, `REFUSED holder="" removed=false`)
		c.Assert(readErr, qt.IsNotNil)
		c.Assert(bodyErr, qt.IsNotNil)
		c.Assert(receipt(claimed), qt.Equals, `REFUSED holder="" removed=false`)
		c.Assert(receipt(removed), qt.Equals, `REFUSED holder="" removed=false`)
		c.Assert(row, qt.Equals, "kept")
	})
	t.Run("a column added to the record", func(t *testing.T) {
		c := testcheck.New(t)
		opened, directory, database := inspectionFixture(c)
		dropRecordAtCleanup(c, opened, database, directory)
		client := newRepeatableClient(c, binary, digest, directory)
		client.prepare(c)
		client.write(c, claim(client.scope, "names", "run-1", "", "digest-1", 1))
		_, err := opened.DB.ExecContext(c.Context(), "ALTER TABLE `"+path.Join(database, directory, repeatableTable)+"` ADD COLUMN unexpected Utf8")
		c.Assert(err, qt.IsNil)

		prepared := client.prepare(c)
		_, readErr := client.client.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{Scope: client.scope, MaxRows: 10})
		failed := client.write(c, fail(client.scope, "names", "run-1", "boom"))

		c.Assert(receipt(prepared), qt.Equals, `REFUSED holder="" removed=false`)
		c.Assert(readErr, qt.IsNotNil)
		c.Assert(receipt(failed), qt.Equals, `REFUSED holder="" removed=false`)
		assertRowCount(c, opened, path.Join(database, directory, repeatableTable), 1)
	})
}

// fillRecord writes rows of the identities from first up to last, exclusive,
// straight into the record, each in the form the provider stores.
func fillRecord(c *qt.C, opened *connection.Connection, table string, first, last int) {
	c.Helper()
	const batch = 1000
	for start := first; start < last; start += batch {
		rows := make([]types.Value, 0, batch)
		for i := start; i < min(start+batch, last); i++ {
			identity := fmt.Sprintf("identity-%05d", i)
			payload, err := repeatables.Encode(&protocol.RepeatableRow{Identity: identity, State: protocol.RepeatableRow_RUNNING,
				AttemptChecksum: "digest", AttemptOrder: 1, RunId: "run-1", AttemptedAtSeconds: 1795000000})
			c.Assert(err, qt.IsNil)
			rows = append(rows, types.StructValue(types.StructFieldValue("identity", types.TextValue(identity)),
				types.StructFieldValue("row", types.BytesValue(payload)), types.StructFieldValue("body", types.BytesValue(make([]byte, 0)))))
		}
		_, err := opened.DB.ExecContext(c.Context(), "DECLARE $rows AS List<Struct<identity: Utf8, row: String, body: String>>; "+
			"UPSERT INTO `"+table+"` SELECT * FROM AS_TABLE($rows)", sql.Named("rows", types.ListValue(rows...)))
		c.Assert(err, qt.IsNil)
	}
}

// A record of MaxRepeatableRows rows is read whole, in identity order, and
// one row more is refused rather than truncated, as is a limit below the
// record's size. The SDK keeps the provider's reason out of the error, so the
// pair of reads over the same rows is what shows the limit decided.
func TestRepeatableRecordReadsTheWholeBound_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	dropRecordAtCleanup(c, opened, database, directory)
	binary, digest := providerBinary(c)
	client := newRepeatableClient(c, binary, digest, directory)
	client.prepare(c)
	table := path.Join(database, directory, repeatableTable)
	fillRecord(c, opened, table, 0, provider.MaxRepeatableRows)

	whole := client.read(c)
	_, belowErr := client.client.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{Scope: client.scope, MaxRows: provider.MaxRepeatableRows - 1})
	fillRecord(c, opened, table, provider.MaxRepeatableRows, provider.MaxRepeatableRows+1)
	_, overErr := client.client.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{Scope: client.scope, MaxRows: provider.MaxRepeatableRows})

	c.Assert(whole.GetRows(), qt.HasLen, provider.MaxRepeatableRows)
	c.Assert(whole.GetRows()[0].GetIdentity(), qt.Equals, "identity-00000")
	c.Assert(whole.GetRows()[provider.MaxRepeatableRows-1].GetIdentity(), qt.Equals, "identity-09999")
	c.Assert(belowErr, qt.ErrorMatches, `provider read the repeatable migration record failed \(FailedPrecondition\)`)
	c.Assert(overErr, qt.ErrorMatches, `provider read the repeatable migration record failed \(FailedPrecondition\)`)
}

// inspectRecord reads directory, which holds the record, through a session of
// a host that lists features, and returns what the read made of it: the
// tables, the reserved objects and the unread tables it names.
func inspectRecord(c *qt.C, binary, digest, directory string, features ...string) (tables, reserved, unread []string) {
	c.Helper()
	client := startProviderListing(c, binary, digest, append(hostFeatures(), features...))
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(c.Context(), request)
	c.Assert(err, qt.IsNil)
	_, err = provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	for _, table := range snapshot.GetTables() {
		tables = append(tables, table.GetIdentity().GetName())
	}
	for _, object := range snapshot.GetReservedObjects() {
		reserved = append(reserved, object.GetObject().GetName()+" "+object.GetPurpose().String())
	}
	for _, table := range snapshot.GetUnreadTables() {
		unread = append(unread, table.GetTable().GetName()+": "+table.GetDiagnostic().GetReason())
	}
	return tables, reserved, unread
}

// A read reports the record as storage the engine keeps for itself to a host
// that lists schema.reserved-repeatable-record, so a plan never drops it. A
// host that does not list it never reads the record as a table of the schema
// either: the read names it as an unread table where the host takes those,
// and leaves it out otherwise.
func TestRepeatableRecordInspection_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	dropRecordAtCleanup(c, opened, database, directory)
	binary, digest := providerBinary(c)
	writer := newRepeatableClient(c, binary, digest, directory)
	writer.prepare(c)
	writer.write(c, claim(writer.scope, "names", "run-1", "", "digest-1", 1))

	reservingTables, reserving, reservingUnread := inspectRecord(c, binary, digest, directory,
		provider.RepeatableRecordFeature, provider.ReservedRepeatableRecordFeature, provider.UnreadTablesFeature)
	namingTables, naming, namingUnread := inspectRecord(c, binary, digest, directory, provider.RepeatableRecordFeature, provider.UnreadTablesFeature)
	silentTables, silent, silentUnread := inspectRecord(c, binary, digest, directory)

	c.Assert(reservingTables, qt.HasLen, 0)
	c.Assert(reserving, qt.DeepEquals, []string{repeatableTable + " REPEATABLE"})
	c.Assert(reservingUnread, qt.HasLen, 0)
	c.Assert(namingTables, qt.HasLen, 0)
	c.Assert(naming, qt.HasLen, 0)
	c.Assert(namingUnread, qt.DeepEquals, []string{repeatableTable + ": " + directory + "/" + repeatableTable +
		" is the record of repeatable migrations, which this host does not read as reserved storage"})
	c.Assert(silentTables, qt.HasLen, 0)
	c.Assert(silent, qt.HasLen, 0)
	c.Assert(silentUnread, qt.HasLen, 0)
}

// The record and its reservation reach a host that lists them, and no other:
// the provider's engine declares each feature only to a host that lists it,
// the reservation only beside the record, and the SDK refuses a call to the
// record from a host that did not list it before the call reaches the
// provider or a database.
func TestRepeatableRecordNegotiationSubprocess(t *testing.T) {
	c := testcheck.New(t)
	binary, digest := providerBinary(c)
	features := func(client *providerruntime.Client) []string {
		return client.Description().GetEngines()[0].GetFeatures()
	}
	both := startProviderListing(c, binary, digest, append(hostFeatures(), provider.RepeatableRecordFeature, provider.ReservedRepeatableRecordFeature))
	record := startProviderListing(c, binary, digest, append(hostFeatures(), provider.RepeatableRecordFeature))
	reservation := startProviderListing(c, binary, digest, append(hostFeatures(), provider.ReservedRepeatableRecordFeature))
	neither := startProviderListing(c, binary, digest, hostFeatures())

	read, err := neither.ReadRepeatables(c.Context(), &protocol.ReadRepeatablesRequest{
		Scope: &protocol.RepeatableScope{SessionId: "unopened", Table: repeatableTable}, MaxRows: provider.MaxRepeatableRows,
	})

	c.Assert(features(both), qt.Contains, provider.RepeatableRecordFeature)
	c.Assert(features(both), qt.Contains, provider.ReservedRepeatableRecordFeature)
	c.Assert(features(record), qt.Contains, provider.RepeatableRecordFeature)
	c.Assert(features(record), qt.Not(qt.Contains), provider.ReservedRepeatableRecordFeature)
	c.Assert(features(reservation), qt.Not(qt.Contains), provider.RepeatableRecordFeature)
	c.Assert(features(reservation), qt.Not(qt.Contains), provider.ReservedRepeatableRecordFeature)
	c.Assert(features(neither), qt.Not(qt.Contains), provider.RepeatableRecordFeature)
	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
	c.Assert(err, qt.ErrorMatches, `.*the repeatable migration record was not negotiated.*`)
	c.Assert(read, qt.IsNil)
}
