//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// executionTables is how many tables the execution of the lock test creates:
// enough that it runs for seconds on any runner, few enough that it ends well
// within the host's 30-second call.
const executionTables = 150

// lockCheck is one CheckLock the test made while an execution ran: what it
// answered, how long it took, and when it returned.
type lockCheck struct {
	outcome  protocol.LockResult_Outcome
	err      error
	took     time.Duration
	answered time.Time
}

// checkLockRepeatedly checks request count times, a pause apart, each within a
// second, as the host's watch of a lock checks it every second.
func checkLockRepeatedly(c *qt.C, client *providerruntime.Client, request *protocol.LockRequest, count int) []lockCheck {
	c.Helper()
	var checks []lockCheck
	for range count {
		time.Sleep(300 * time.Millisecond)
		ctx, cancel := context.WithTimeout(c.Context(), time.Second)
		started := time.Now()
		result, err := client.CheckLock(ctx, request)
		cancel()
		checks = append(checks, lockCheck{outcome: result.GetOutcome(), err: err, took: time.Since(started), answered: time.Now()})
	}
	return checks
}

// The provider answers a check of a lock while an execution of the same
// session runs. The host's watch checks its lock every second and counts a
// check unanswered for ten seconds as a lost lock, and an execution that
// creates hundreds of tables runs longer than that. The provider answered a
// check only once the execution ended, so a host apply that created 500
// tables lost its lock on 25.1.4.7 and 26.2.1.14 (`advisory lock
// "ptah_schema_apply" on ydb was lost while it was held`), where Ptah's
// built-in YDB engine kept it.
func TestLockCheckIsAnsweredDuringAnExecution(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	binary, digest := providerBinary(c)
	client, session, _ := historySession(c, binary, digest)
	request := nativeLockRequest(session, "apply", "schema apply")
	acquireNativeLock(c, client, request)
	units := make([]*protocol.Unit, executionTables)
	for i := range units {
		table := path.Join(database, directory, fmt.Sprintf("t%03d", i))
		units[i] = &protocol.Unit{ChangeId: strconv.Itoa(i), Body: &protocol.Unit_Sql{Sql: "CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))"}}
	}
	type execution struct {
		result *protocol.Execution
		err    error
		ended  time.Time
	}
	executed := make(chan execution, 1)
	go func() {
		result, err := client.Execute(context.Background(), &protocol.ExecuteRequest{SessionId: session, RequestId: "create", Units: units})
		executed <- execution{result: result, err: err, ended: time.Now()}
	}()

	checks := checkLockRepeatedly(c, client, request, 3)
	done := <-executed

	c.Assert(done.err, qt.IsNil)
	c.Assert(done.result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(done.result.AppliedCount, qt.Equals, uint32(executionTables))
	for i, check := range checks {
		c.Assert(check.err, qt.IsNil, qt.Commentf("check %d took %s", i, check.took))
		c.Assert(check.outcome, qt.Equals, protocol.LockResult_ACQUIRED, qt.Commentf("check %d", i))
		c.Assert(check.answered.Before(done.ended), qt.IsTrue, qt.Commentf("check %d answered %s after the execution ended", i, check.answered.Sub(done.ended)))
	}
	released, err := client.ReleaseLock(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(released.Outcome, qt.Equals, protocol.LockResult_RELEASED)
}
