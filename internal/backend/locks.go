package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// lockHold permits deterministic lease-loss tests without changing production
// acquisition. The real hold uses strict native ownership reads and a watchdog.
type lockHold interface {
	Check(context.Context) error
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
	if record := s.locks[request.Scope.LockId]; record != nil && record.name != request.Scope.Name {
		s.mu.Unlock()
		return nil, nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB lock handle names a different lock")
	}
	return s, s.mu.Unlock, nil
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
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return failedLock(request, record.outcome, "the YDB session lost its lock guarantee"), nil
	}
	defer stop()
	record.outcome = protocol.LockResult_UNKNOWN
	hold, busy, err := locking.Try(guarded, s.connection.Driver, s.database, request.Scope.Name)
	if refused, ok := refusedLock(request, record, err); ok {
		return refused, nil
	}
	if errors.Is(err, locking.ErrRelaxedNode) {
		// The node's settings were read before any session opened on it, so
		// the handle holds nothing and the session keeps no uncertainty.
		record.outcome = protocol.LockResult_NOT_ACQUIRED
		return failedLock(request, record.outcome, err.Error()), nil
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
	if s.verifyLocks(ctx) != nil {
		record.outcome = protocol.LockResult_UNKNOWN
	}
	return lockResult(request, record.outcome), nil
}

// CheckLock verifies native ownership rather than returning a cached receipt.
func (p *Provider) CheckLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	s, unlock, err := p.lockSession(request)
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
func (s *session) checkLock(ctx context.Context, record *lockRecord) error {
	err := record.hold.Check(ctx)
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
func (s *session) observeLockLoss() bool {
	for _, record := range s.locks {
		if record.hold != nil && !record.released && record.hold.Context().Err() != nil {
			s.fenced = true
		}
	}
	return s.fenced
}
func (s *session) verifyLocks(ctx context.Context) error {
	if s.observeLockLoss() {
		return runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session lost its lock guarantee")
	}
	for _, record := range s.locks {
		if record.hold != nil && !record.released {
			if s.checkLock(ctx, record) != nil {
				return runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session lost its lock guarantee")
			}
		}
	}
	return nil
}
func (s *session) guard(ctx context.Context) (context.Context, func(), error) {
	if err := s.verifyLocks(ctx); err != nil {
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
