package locking

// A controlled clock and native-session seam establish expiration and uncertain
// responses deterministically; live tests cover actual server ownership.
import (
	"context"
	"errors"
	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination/options"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSession struct {
	description *coordination.SemaphoreDescription
	describeErr error
	closeErr    error
	calls       atomic.Int32
	closes      atomic.Int32
	onDescribe  func()
}

func (s *fakeSession) SessionID() uint64 { return 7 }
func (s *fakeSession) DescribeSemaphore(context.Context, string, ...options.DescribeSemaphoreOption) (*coordination.SemaphoreDescription, error) {
	s.calls.Add(1)
	if s.onDescribe != nil {
		s.onDescribe()
	}
	return s.description, s.describeErr
}
func (s *fakeSession) Close(context.Context) error { s.closes.Add(1); return s.closeErr }

type fakeLease struct{ ctx context.Context }

func (l fakeLease) Context() context.Context { return l.ctx }
func ownedDescription() *coordination.SemaphoreDescription {
	return &coordination.SemaphoreDescription{Name: "lock", Ephemeral: true, Count: coordination.Exclusive, Limit: coordination.Exclusive, Owners: []*coordination.SemaphoreSession{{SessionID: 7, Count: coordination.Exclusive}}}
}
func TestHoldRejectsExpiredConfirmationBeforeRefresh(t *testing.T) {
	c := qt.New(t)
	now := time.Now()
	native := &fakeSession{description: ownedDescription()}
	h := newHold(native, fakeLease{context.Background()}, "lock", now, func() time.Time { return now }, func(context.Context) error { return nil })
	c.Assert(h.Check(c.Context()), qt.IsNil)
	now = now.Add(freshness)
	c.Assert(h.Check(c.Context()), qt.ErrorIs, ErrUnknown)
	c.Assert(native.calls.Load(), qt.Equals, int32(1))
	c.Assert(h.Context().Err(), qt.ErrorIs, context.Canceled)
	now = now.Add(-freshness)
	c.Assert(h.Check(c.Context()), qt.ErrorIs, ErrUnknown)
	c.Assert(native.calls.Load(), qt.Equals, int32(1))
}
func TestHoldRequiresExactFreshExclusiveOwnership(t *testing.T) {
	for _, name := range []string{"missing", "different owner", "shared", "durable", "wrong name", "query error", "stale reply", "expired lease", "node config"} {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			now := time.Now()
			native := &fakeSession{description: ownedDescription()}
			lease, cancel := context.WithCancel(context.Background())
			defer cancel()
			validate := func(context.Context) error { return nil }
			want := ErrLost
			switch name {
			case "missing":
				native.description = nil
			case "different owner":
				native.description.Owners[0].SessionID = 8
			case "shared":
				native.description.Owners[0].Count = 1
			case "durable":
				native.description.Ephemeral = false
			case "wrong name":
				native.description.Name = "other"
			case "query error":
				native.describeErr = errors.New("transport")
				want = ErrUnknown
			case "stale reply":
				native.onDescribe = func() { now = now.Add(freshness) }
				want = ErrUnknown
			case "expired lease":
				cancel()
			case "node config":
				validate = func(context.Context) error { return errors.New("relaxed") }
				want = ErrUnknown
			}
			h := newHold(native, fakeLease{lease}, "lock", now, func() time.Time { return now }, validate)
			c.Assert(h.Check(c.Context()), qt.ErrorIs, want)
			native.description = ownedDescription()
			native.describeErr = nil
			native.onDescribe = nil
			c.Assert(h.Check(c.Context()), qt.ErrorIs, want)
			c.Assert(h.Context().Err(), qt.ErrorIs, context.Canceled)
		})
	}
}
func TestHoldCloseRetainsFailureAndClosesOnce(t *testing.T) {
	c := qt.New(t)
	native := &fakeSession{description: ownedDescription(), closeErr: errors.New("lost reply")}
	h := newHold(native, fakeLease{context.Background()}, "lock", time.Now(), time.Now, func(context.Context) error { return nil })
	h.start()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = h.Close(c.Context()) })
	}
	wg.Wait()
	for _, err := range errs {
		c.Assert(err, qt.IsNotNil)
	}
	c.Assert(native.closes.Load(), qt.Equals, int32(1))
	c.Assert(h.Context().Err(), qt.ErrorIs, context.Canceled)
}
func TestWatchCancelsWorkWhenNativeLeaseEnds(t *testing.T) {
	c := qt.New(t)
	lease, cancel := context.WithCancel(context.Background())
	native := &fakeSession{description: ownedDescription()}
	h := newHold(native, fakeLease{lease}, "lock", time.Now(), time.Now, func(context.Context) error { return nil })
	h.start()
	defer func() { c.Check(h.Close(context.Background()), qt.IsNil) }()
	cancel()
	select {
	case <-h.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("lease loss did not stop guarded work")
	}
	c.Assert(h.Check(c.Context()), qt.ErrorIs, ErrLost)
}
