package backend

// These tests inject ownership loss at database dispatch and commit boundaries.
// The production native hold is exercised separately against both YDB versions.
import (
	"context"
	"errors"
	"fmt"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
	"sync/atomic"
	"testing"
	"time"
)

type lockProbe struct {
	ctx     context.Context
	cancel  context.CancelFunc
	failure error
	closes  atomic.Int32
}

func (l *lockProbe) Check(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if l.ctx.Err() != nil {
		return locking.ErrLost
	}
	return l.failure
}
func (l *lockProbe) Context() context.Context        { return l.ctx }
func (l *lockProbe) Close(ctx context.Context) error { l.closes.Add(1); l.cancel(); return ctx.Err() }
func attachLock(s *session) *lockProbe {
	ctx, cancel := context.WithCancel(context.Background())
	hold := &lockProbe{ctx: ctx, cancel: cancel}
	s.locks = map[string]*lockRecord{"lock": {name: "migration", outcome: protocol.LockResult_ACQUIRED, hold: hold}}
	return hold
}
func lockRequest() *protocol.LockRequest {
	return &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: "session", LockId: "lock", Name: "migration"}, RequestId: "request"}
}
func TestLostLockFencesExecutionHistoryAndCommit(t *testing.T) {
	c := testcheck.New(t)
	driver := &driverProbe{}
	p := probeProvider(c, driver)
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(err, qt.IsNil)
	p.sessions["session"].database = "/local"
	hold := attachLock(p.sessions["session"])
	hold.failure = locking.ErrUnknown
	result, err := p.Execute(c.Context(), sqlRequest("tx"))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(driver.execCalls.Load(), qt.Equals, int32(0))
	// Even a later apparently healthy native check cannot revive this session.
	hold.failure = nil
	scope := &protocol.HistoryScope{SessionId: "session", Table: "revisions", Format: protocol.HistoryScope_NATIVE, TransactionId: "tx"}
	mutation, err := p.MutateHistory(c.Context(), &protocol.MutateHistoryRequest{Scope: scope, RequestId: "history", Replacement: &protocol.HistoryRevision{Version: "1", State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_UP}})
	c.Assert(err, qt.IsNil)
	c.Assert(mutation.Outcome, qt.Equals, protocol.HistoryResult_REFUSED)
	_, err = p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
	c.Assert(err, qt.IsNotNil)
	c.Assert(driver.commitCalls.Load(), qt.Equals, int32(0))
	rolled, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))
	c.Assert(err, qt.IsNil)
	c.Assert(rolled.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	released, err := p.ReleaseLock(c.Context(), lockRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(released.Outcome, qt.Equals, protocol.LockResult_UNKNOWN)
	_, err = p.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: "session", TransactionId: "new"})
	c.Assert(err, qt.IsNotNil)
}
func TestLockReleaseIsBoundAndFencesLateAcquisition(t *testing.T) {
	c := testcheck.New(t)
	p := probeProvider(c, &driverProbe{})
	hold := attachLock(p.sessions["session"])
	wrong := proto.CloneOf(lockRequest())
	wrong.Scope.Name = "other"
	_, err := p.ReleaseLock(c.Context(), wrong)
	c.Assert(err, qt.IsNotNil)
	c.Assert(hold.closes.Load(), qt.Equals, int32(0))
	for range 2 {
		r, err := p.ReleaseLock(c.Context(), lockRequest())
		c.Assert(err, qt.IsNil)
		c.Assert(r.Outcome, qt.Equals, protocol.LockResult_RELEASED)
	}
	c.Assert(hold.closes.Load(), qt.Equals, int32(1))
	unused := proto.CloneOf(lockRequest())
	unused.Scope.LockId = "unused"
	r, err := p.ReleaseLock(c.Context(), unused)
	c.Assert(err, qt.IsNil)
	c.Assert(r.Outcome, qt.Equals, protocol.LockResult_NOT_ACQUIRED)
	_, err = p.TryLock(c.Context(), unused)
	c.Assert(err, qt.IsNotNil)
}
func TestLockLossDuringCommitRetainsUnknown(t *testing.T) {
	c := testcheck.New(t)
	driver := &driverProbe{}
	p := probeProvider(c, driver)
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(err, qt.IsNil)
	hold := attachLock(p.sessions["session"])
	driver.onCommit = func(context.Context) error { hold.cancel(); return nil }
	result, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	again, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
	c.Assert(err, qt.IsNil)
	c.Assert(again.Outcome, qt.Equals, protocol.TransactionResult_UNKNOWN)
	c.Assert(driver.commitCalls.Load(), qt.Equals, int32(1))
}

func TestLockLossCancelsDispatchedSQLAndRetainsUncertainty(t *testing.T) {
	c := testcheck.New(t)
	driver := &driverProbe{}
	p := probeProvider(c, driver)
	hold := attachLock(p.sessions["session"])
	driver.onExec = func(ctx context.Context) error {
		hold.cancel()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Error("guard did not cancel dispatched SQL")
		}
		return nil // An acknowledgment can race ownership loss; it cannot prove safety.
	}
	result, err := p.Execute(c.Context(), sqlRequest(""))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(driver.execCalls.Load(), qt.Equals, int32(1))
	again, err := p.Execute(c.Context(), sqlRequest(""))
	c.Assert(err, qt.IsNil)
	c.Assert(again.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(driver.execCalls.Load(), qt.Equals, int32(1))
}

func TestCanceledReleaseStillClosesOwnedNativeSession(t *testing.T) {
	c := testcheck.New(t)
	p := probeProvider(c, &driverProbe{})
	hold := attachLock(p.sessions["session"])
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err := p.ReleaseLock(ctx, lockRequest())
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.LockResult_RELEASED)
	c.Assert(hold.closes.Load(), qt.Equals, int32(1))
}

// A refusal locking.Try reports before it asked for the semaphore is a
// confirmed NOT_ACQUIRED receipt with the refusal's own text as its reason,
// never a driver's text an outer error wraps around it.
func TestRefusedLockIsAConfirmedReceipt_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"a right the user lacks", authored.Wrap(locking.ErrRefused, "this user may not use the YDB coordination node /local/ptah_locks")},
		{"a node that is not strict", authored.Wrap(locking.ErrRelaxedNode, "this user may not use the YDB coordination node /local/ptah_locks")},
		{"a refusal an outer error wraps", fmt.Errorf("dial grpc://admin:hunter2@db:2136: %w", // #nosec G101 -- a fake credential the reason must not carry
			authored.Wrap(locking.ErrRefused, "this user may not use the YDB coordination node /local/ptah_locks"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			record := &lockRecord{name: "migration", outcome: protocol.LockResult_UNKNOWN}

			result, refused := refusedLock(lockRequest(), record, test.err)

			c.Assert(refused, qt.IsTrue)
			c.Assert(result.GetOutcome(), qt.Equals, protocol.LockResult_NOT_ACQUIRED)
			c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, "this user may not use the YDB coordination node /local/ptah_locks")
			c.Assert(result.GetScope().GetLockId(), qt.Equals, "lock")
			c.Assert(record.outcome, qt.Equals, protocol.LockResult_NOT_ACQUIRED)
		})
	}
}

// Any other failure of locking.Try is not a refusal: the caller keeps the
// handle's outcome UNKNOWN and fences the session.
func TestRefusedLockIsAConfirmedReceipt_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"no failure", nil},
		{"a failure after the semaphore was asked for", errors.New("acquire YDB semaphore or close its session failed")},
		{"a lost lock", locking.ErrLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			record := &lockRecord{name: "migration", outcome: protocol.LockResult_UNKNOWN}

			result, refused := refusedLock(lockRequest(), record, test.err)

			c.Assert(refused, qt.IsFalse)
			c.Assert(result, qt.IsNil)
			c.Assert(record.outcome, qt.Equals, protocol.LockResult_UNKNOWN)
		})
	}
}
