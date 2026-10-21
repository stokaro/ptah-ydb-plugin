package locking

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination/options"
)

// ErrLost means the native lease ended or the server reports another owner.
var ErrLost = errors.New("YDB lock ownership was lost")

// ErrUnknown means ownership could not be confirmed within its freshness bound.
var ErrUnknown = errors.New("YDB lock ownership is unconfirmed")

type nativeSession interface {
	DescribeSemaphore(context.Context, string, ...options.DescribeSemaphoreOption) (*coordination.SemaphoreDescription, error)
	SessionID() uint64
	Close(context.Context) error
}
type nativeLease interface{ Context() context.Context }

// Hold owns a dedicated coordination session. A failed check is permanent;
// neither a later server reply nor stream recovery can resurrect its guarantee.
type Hold struct {
	session   nativeSession
	lease     nativeLease
	name      string
	now       func() time.Time
	validate  func(context.Context) error
	mu        sync.Mutex
	confirmed time.Time
	failure   error
	closed    bool
	context   context.Context
	cancel    context.CancelFunc
	stop      context.CancelFunc
	watching  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newHold(session nativeSession, lease nativeLease, name string, confirmed time.Time, now func() time.Time, validate func(context.Context) error) *Hold {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hold{session: session, lease: lease, name: name, now: now, validate: validate, confirmed: confirmed, context: ctx, cancel: cancel}
}

// Context ends permanently on loss, uncertainty, or explicit close. Guarded
// database calls must use a context canceled when this context ends.
func (h *Hold) Context() context.Context { return h.context }

// Check verifies strict native ownership and rejects an expired confirmation
// before asking again. This prevents a paused process from reviving a stale lease.
func (h *Hold) Check(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil {
		return h.failure
	}
	if h.closed || h.lease.Context().Err() != nil {
		return h.fail(ErrLost)
	}
	asked := h.now()
	if confirmationExpired(asked, h.confirmed) {
		return h.fail(ErrUnknown)
	}
	bounded, cancel := context.WithTimeout(ctx, checkInterval)
	defer cancel()
	if err := h.validate(bounded); err != nil {
		return h.fail(ErrUnknown)
	}
	description, err := h.session.DescribeSemaphore(bounded, h.name, options.WithDescribeOwners(true))
	if err != nil || bounded.Err() != nil || confirmationExpired(h.now(), asked) {
		return h.fail(ErrUnknown)
	}
	if h.lease.Context().Err() != nil {
		return h.fail(ErrLost)
	}
	if description == nil || description.Name != h.name || !description.Ephemeral || description.Limit != coordination.Exclusive ||
		description.Count != coordination.Exclusive || len(description.Owners) != 1 || description.Owners[0] == nil ||
		description.Owners[0].SessionID != h.session.SessionID() || description.Owners[0].Count != coordination.Exclusive {
		return h.fail(ErrLost)
	}
	h.confirmed = asked
	return nil
}
func (h *Hold) fail(err error) error { h.failure = err; h.cancel(); return err }
func (h *Hold) start() {
	ctx, stop := context.WithCancel(context.Background())
	h.stop = stop
	h.watching = make(chan struct{})
	go func() {
		defer close(h.watching)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.lease.Context().Done():
				h.mu.Lock()
				if !h.closed && h.failure == nil {
					_ = h.fail(ErrLost) // Check returns the retained failure.
				}
				h.mu.Unlock()
				return
			case <-ticker.C:
				if h.Check(ctx) != nil {
					return
				}
			}
		}
	}()
}

// Close stops the watch and closes the dedicated native session once. Its
// acknowledgment confirms semaphore release and session stop through the native
// stream; no unbounded Lease.Release goroutine is needed. Cleanup errors remain errors on repetition.
func (h *Hold) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.cancel()
		h.mu.Unlock()
		if h.stop != nil {
			h.stop()
			<-h.watching
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if h.session.Close(bounded) != nil {
			h.closeErr = fmt.Errorf("close YDB lock session failed")
		}
	})
	return h.closeErr
}

// Wall time also advances across machine suspension on systems whose monotonic
// clock stops during sleep. Either elapsed clock can invalidate a confirmation.
func confirmationExpired(now, before time.Time) bool {
	return now.Sub(before) >= freshness || now.Round(0).Sub(before.Round(0)) >= freshness
}
