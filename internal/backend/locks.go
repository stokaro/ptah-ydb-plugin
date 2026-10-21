package backend

import (
	"context"
	"errors"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func (s *session) reserveLock(scope *protocol.LockScope) (*lockRecord, error) {
	if s.locks[scope.LockId] != nil {
		return nil, status.Error(codes.AlreadyExists, "YDB lock handle was already reserved")
	}
	if len(s.locks) >= lockHandleLimit {
		return nil, status.Error(codes.ResourceExhausted, "YDB lock handle limit reached; open another session")
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
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, err := p.lookup(request.Scope.SessionId)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	}
	if record := s.locks[request.Scope.LockId]; record != nil && record.name != request.Scope.Name {
		s.mu.Unlock()
		return nil, nil, status.Error(codes.InvalidArgument, "YDB lock handle names a different lock")
	}
	return s, s.mu.Unlock, nil
}
func lockResult(request *protocol.LockRequest, outcome protocol.LockResult_Outcome) *protocol.LockResult {
	result := &protocol.LockResult{Scope: proto.CloneOf(request.Scope), RequestId: request.RequestId, Outcome: outcome}
	return result
}

// TryLock reserves a single-use handle before creating or contacting native
// coordination storage. Only a confirmed busy result permits another attempt.
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
	if ctx.Err() != nil || s.active != "" || s.fenced {
		return lockResult(request, record.outcome), nil
	}
	active := 0
	for _, existing := range s.locks {
		if existing.hold != nil && !existing.released {
			active++
		}
	}
	if active >= 16 {
		return lockResult(request, record.outcome), nil
	}
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		return lockResult(request, record.outcome), nil
	}
	defer stop()
	record.outcome = protocol.LockResult_UNKNOWN
	hold, busy, err := locking.Try(guarded, s.connection.Driver, s.database, request.Scope.Name)
	if err != nil {
		s.fenced = true
		return lockResult(request, record.outcome), nil
	}
	if busy {
		record.outcome = protocol.LockResult_BUSY
		return lockResult(request, record.outcome), nil
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
		return status.Error(codes.FailedPrecondition, "YDB session lost its lock guarantee")
	}
	for _, record := range s.locks {
		if record.hold != nil && !record.released {
			if s.checkLock(ctx, record) != nil {
				return status.Error(codes.FailedPrecondition, "YDB session lost its lock guarantee")
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
