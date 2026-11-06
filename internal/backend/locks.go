package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
)

// lockHold permits deterministic lease-loss tests without changing production
// acquisition. The real hold uses strict native ownership reads and a watchdog.
type lockHold interface {
	Check(context.Context) error
	// Confirm is Check that trusts a confirmation the hold's watch made
	// within the last second.
	Confirm(context.Context) error
	Context() context.Context
	Close(context.Context) error
}
type lockRecord struct {
	name     string
	outcome  protocol.LockResult_Outcome
	hold     lockHold
	released bool
}

const lockHandleLimit = 1024

// heldLockLimit is the most locks one session holds at once.
const heldLockLimit = 16

// reserveLock records a handle for scope. The caller holds lockMu.
func (s *session) reserveLock(scope *protocol.LockScope) (*lockRecord, error) {
	if s.locks[scope.LockId] != nil {
		return nil, runtime.ErrorWithReason(codes.AlreadyExists, "YDB lock handle was already reserved")
	}
	if len(s.locks) >= lockHandleLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "YDB lock handle limit reached; open another session")
	}
	if s.locks == nil {
		s.locks = make(map[string]*lockRecord)
	}
	record := &lockRecord{name: scope.Name, outcome: protocol.LockResult_NOT_ACQUIRED}
	s.locks[scope.LockId] = record
	return record, nil
}

// lockSession returns the session request names holding mu and lockMu, for
// an acquisition or a release, and the function that unlocks both.
func (p *Provider) lockSession(request *protocol.LockRequest) (*session, func(), error) {
	if err := provider.ValidateLockRequest(request); err != nil {
		return nil, nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.Scope.SessionId)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
	}
	s.lockMu.Lock()
	unlock := func() { s.lockMu.Unlock(); s.mu.Unlock() }
	if err := s.namesItsLock(request); err != nil {
		unlock()
		return nil, nil, err
	}
	return s, unlock, nil
}

// lockState returns the session request names holding lockMu alone, for a
// check, which an execution holding mu does not delay, and the function that
// unlocks it.
func (p *Provider) lockState(request *protocol.LockRequest) (*session, func(), error) {
	if err := provider.ValidateLockRequest(request); err != nil {
		return nil, nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.Scope.SessionId)
	if err != nil {
		return nil, nil, err
	}
	s.lockMu.Lock()
	if s.locksClosed {
		s.lockMu.Unlock()
		return nil, nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
	}
	if err := s.namesItsLock(request); err != nil {
		s.lockMu.Unlock()
		return nil, nil, err
	}
	return s, s.lockMu.Unlock, nil
}

// namesItsLock refuses a request whose handle the session reserved for
// another lock name. The caller holds lockMu.
func (s *session) namesItsLock(request *protocol.LockRequest) error {
	if record := s.locks[request.Scope.LockId]; record != nil && record.name != request.Scope.Name {
		return runtime.ErrorWithReason(codes.InvalidArgument, "YDB lock handle names a different lock")
	}
	return nil
}
func lockResult(request *protocol.LockRequest, outcome protocol.LockResult_Outcome) *protocol.LockResult {
	result := &protocol.LockResult{Scope: proto.CloneOf(request.Scope), RequestId: request.RequestId, Outcome: outcome}
	return result
}

// failedLock is a TryLock receipt with an outcome other than ACQUIRED and
// the reason for it, which names the session's state and never a value. The
// SDK sends the reason only to a host that lists errors.reasons.
func failedLock(request *protocol.LockRequest, outcome protocol.LockResult_Outcome, reason string) *protocol.LockResult {
	result := lockResult(request, outcome)
	result.Diagnostic = &protocol.ProviderDiagnostic{Reason: reason}
	return result
}

// refusedLock is the receipt for an acquisition locking.Try refused before it
// asked for the semaphore, and reports whether err is one: the server refused
// this user the lock node or a session on it (locking.ErrRefused), or the node
// is not strict (locking.ErrRelaxedNode). The handle holds nothing and the
// session keeps no uncertainty, so the receipt is a confirmed NOT_ACQUIRED with
// the refusal's reason. An error status instead would reach the host as an
// unknown outcome, which fences its session and sends a cleanup release.
func refusedLock(request *protocol.LockRequest, record *lockRecord, err error) (*protocol.LockResult, bool) {
	if !errors.Is(err, locking.ErrRefused) && !errors.Is(err, locking.ErrRelaxedNode) {
		return nil, false
	}
	record.outcome = protocol.LockResult_NOT_ACQUIRED
	var reason string
	if refusal, ok := errors.AsType[*authored.Error](err); ok {
		reason = refusal.Error()
	}
	return failedLock(request, record.outcome, reason), true
}

// TryLock reserves a single-use handle before creating or contacting native
// coordination storage. Only a confirmed busy result permits another attempt.
// A server that refuses this user the lock node or its session, and a node
// that is not strict, are answered NOT_ACQUIRED with a reason that names the
// missing right or the node's settings (see refusedLock).
func (p *Provider) TryLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	s, unlock, err := p.lockSession(request)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record, err := s.reserveLock(request.Scope)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return lockResult(request, record.outcome), nil
	}
	if s.active != "" {
		return failedLock(request, record.outcome, "a YDB session takes no lock while it has an active transaction"), nil
	}
	if s.fenced {
		return failedLock(request, record.outcome, "the YDB session lost a lock guarantee and takes no further lock"), nil
	}
	active := 0
	for _, existing := range s.locks {
		if existing.hold != nil && !existing.released {
			active++
		}
	}
	if active >= heldLockLimit {
		return failedLock(request, record.outcome, fmt.Sprintf("the YDB session already holds %d locks, the most one session holds", heldLockLimit)), nil
	}
	guarded, stop, err := s.guardLocked(ctx)
	if err != nil {
		return failedLock(request, record.outcome, "the YDB session lost its lock guarantee"), nil
	}
	defer stop()
	record.outcome = protocol.LockResult_UNKNOWN
	hold, busy, err := s.tryWithoutLockState(guarded, request.Scope.Name)
	if refused, ok := refusedLock(request, record, err); ok {
		return refused, nil
	}
	if err != nil {
		s.fenced = true
		return lockResult(request, record.outcome), nil
	}
	if busy {
		record.outcome = protocol.LockResult_BUSY
		return failedLock(request, record.outcome, "another YDB session holds the lock"), nil
	}
	record.hold = hold
	record.outcome = protocol.LockResult_ACQUIRED
	if s.verifyLocksLocked(ctx) != nil {
		record.outcome = protocol.LockResult_UNKNOWN
	}
	return lockResult(request, record.outcome), nil
}

// tryWithoutLockState takes the lock name on the server with lockMu
// released, so the checks of the session's other locks are answered while
// the acquisition waits for the server. The caller holds mu, which keeps
// every other acquisition, release and execution out, and holds lockMu
// again when it returns.
func (s *session) tryWithoutLockState(ctx context.Context, name string) (*locking.Hold, bool, error) {
	s.lockMu.Unlock()
	defer s.lockMu.Lock()
	return locking.Try(ctx, s.connection.Driver, s.database, name)
}

// CheckLock verifies native ownership rather than returning a cached receipt.
// It holds lockMu alone (see lockState).
func (p *Provider) CheckLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	s, unlock, err := p.lockState(request)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record := s.locks[request.Scope.LockId]
	if record == nil {
		return lockResult(request, protocol.LockResult_NOT_ACQUIRED), nil
	}
	if record.hold != nil && !record.released {
		_ = s.checkLock(ctx, record) // The receipt retains the exact failure outcome.
	}
	outcome := record.outcome
	if outcome == protocol.LockResult_BUSY {
		outcome = protocol.LockResult_NOT_ACQUIRED
	}
	return lockResult(request, outcome), nil
}

// checkLock checks record's hold and fences the session when the check
// fails. The caller holds lockMu.
func (s *session) checkLock(ctx context.Context, record *lockRecord) error {
	return s.settleLock(record, record.hold.Check(ctx))
}

// settleLock records the failure of a check of record: the session is
// fenced, and the lock reads as lost or unknown. The caller holds lockMu.
func (s *session) settleLock(record *lockRecord, err error) error {
	if err != nil {
		s.fenced = true
		record.outcome = protocol.LockResult_UNKNOWN
		if errors.Is(err, locking.ErrLost) {
			record.outcome = protocol.LockResult_LOST
		}
	}
	return err
}

// ReleaseLock fences unused handles and releases only the native session bound
// to this exact name and handle. Uncertainty is retained after cleanup.
func (p *Provider) ReleaseLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	s, unlock, err := p.lockSession(request)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record := s.locks[request.Scope.LockId]
	if record == nil {
		record, err = s.reserveLock(request.Scope)
		if err != nil {
			return nil, err
		}
	}
	if record.hold != nil && !record.released {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		_ = s.checkLock(cleanup, record) // Cleanup survives caller cancellation.
		err := record.hold.Close(cleanup)
		record.released = true
		if err != nil {
			s.fenced = true
			record.outcome = protocol.LockResult_UNKNOWN
			s.cleanupErr = errors.Join(s.cleanupErr, err)
		} else if record.outcome == protocol.LockResult_ACQUIRED {
			record.outcome = protocol.LockResult_RELEASED
		}
	}
	outcome := record.outcome
	if outcome == protocol.LockResult_BUSY {
		outcome = protocol.LockResult_NOT_ACQUIRED
	}
	return lockResult(request, outcome), nil
}

func (s *session) closeLocks() {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	s.locksClosed = true
	// Each dedicated session receives its own bounded cleanup opportunity.
	for _, record := range s.locks {
		if record.hold == nil || record.released {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := record.hold.Close(ctx)
		cancel()
		record.released = true
		s.cleanupErr = errors.Join(s.cleanupErr, err)
	}
}

// observeLockLoss fences the session when a lease ended. The caller holds
// lockMu.
func (s *session) observeLockLoss() bool {
	for _, record := range s.locks {
		if record.hold != nil && !record.released && record.hold.Context().Err() != nil {
			s.fenced = true
		}
	}
	return s.fenced
}

// verifyLocks checks every lock the session holds; see verifyLocksLocked.
func (s *session) verifyLocks(ctx context.Context) error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.verifyLocksLocked(ctx)
}

// confirmLocks is verifyLocks before one statement of an execution, which
// takes a confirmation the watch of a hold made within the last second rather
// than asking the server before every statement (see locking.Hold.Confirm):
// two calls per statement, one of them to the scheme service that a long
// schema change keeps busy, made a creation of 500 tables slower than Ptah's
// built-in YDB engine, which asked between statements only once its last
// answer was five seconds old.
func (s *session) confirmLocks(ctx context.Context) error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.verifyEachLock(func(record *lockRecord) error { return s.settleLock(record, record.hold.Confirm(ctx)) })
}

// verifyLocksLocked checks every lock the session holds and fences the
// session on a loss or an unconfirmed check. The caller holds lockMu.
func (s *session) verifyLocksLocked(ctx context.Context) error {
	return s.verifyEachLock(func(record *lockRecord) error { return s.checkLock(ctx, record) })
}

func (s *session) verifyEachLock(check func(*lockRecord) error) error {
	if s.observeLockLoss() {
		return runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session lost its lock guarantee")
	}
	for _, record := range s.locks {
		if record.hold != nil && !record.released {
			if check(record) != nil {
				return runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session lost its lock guarantee")
			}
		}
	}
	return nil
}

// guard verifies the session's locks and returns a context that ends when
// any of their holds ends; see guardLocked.
func (s *session) guard(ctx context.Context) (context.Context, func(), error) {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.guardLocked(ctx)
}

// guardLocked is guard for a caller that holds lockMu.
func (s *session) guardLocked(ctx context.Context) (context.Context, func(), error) {
	if err := s.verifyLocksLocked(ctx); err != nil {
		return nil, nil, err
	}
	guarded, cancel := context.WithCancel(ctx)
	var stops []func() bool
	for _, record := range s.locks {
		if record.hold != nil && !record.released {
			stops = append(stops, context.AfterFunc(record.hold.Context(), cancel))
		}
	}
	stop := func() {
		for _, f := range stops {
			f()
		}
		cancel()
	}
	return guarded, stop, nil
}
