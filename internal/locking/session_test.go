package locking

import (
	"context"
	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	wire "github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"testing"
)

// The stream seam injects native replies that a healthy live server cannot
// produce on demand. It verifies status handling, exact IDs, and cleanup facts.
type replyStream struct {
	Ydb_Coordination_V1.CoordinationService_SessionClient
	send func(*wire.SessionRequest) error
}

func (s replyStream) Send(request *wire.SessionRequest) error { return s.send(request) }
func testSession(c *qt.C, reply func(*wire.SessionRequest) *wire.SessionResponse) *wireSession {
	c.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.Cleanup(cancel)
	s := &wireSession{ctx: ctx, cancel: cancel, id: 7, responses: make(chan *wire.SessionResponse, 8)}
	s.stream = replyStream{send: func(request *wire.SessionRequest) error { s.responses <- reply(request); return nil }}
	return s
}
func TestAcquisitionStatusCannotBecomeBusyOrSuccess(t *testing.T) {
	for _, code := range []Ydb.StatusIds_StatusCode{Ydb.StatusIds_UNAUTHORIZED, Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_UNAVAILABLE} {
		for _, acquired := range []bool{true, false} {
			c := qt.New(t)
			s := testSession(c, func(request *wire.SessionRequest) *wire.SessionResponse {
				return &wire.SessionResponse{Response: &wire.SessionResponse_AcquireSemaphoreResult_{AcquireSemaphoreResult: &wire.SessionResponse_AcquireSemaphoreResult{ReqId: request.GetAcquireSemaphore().ReqId, Status: code, Acquired: acquired}}}
			})
			held, err := s.acquire(c.Context(), "lock")
			c.Assert(err, qt.IsNotNil)
			c.Assert(held, qt.IsFalse)
		}
	}
	c := qt.New(t)
	s := testSession(c, func(request *wire.SessionRequest) *wire.SessionResponse {
		return &wire.SessionResponse{Response: &wire.SessionResponse_AcquireSemaphoreResult_{AcquireSemaphoreResult: &wire.SessionResponse_AcquireSemaphoreResult{ReqId: request.GetAcquireSemaphore().ReqId, Status: Ydb.StatusIds_SUCCESS}}}
	})
	held, err := s.acquire(c.Context(), "lock")
	c.Assert(err, qt.IsNil)
	c.Assert(held, qt.IsFalse)
}
func TestNativeReleaseRequiresBothAcknowledgments(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    Ydb.StatusIds_StatusCode
		released  bool
		stopID    uint64
		wantError bool
	}{
		{"confirmed", Ydb.StatusIds_SUCCESS, true, 7, false},
		{"permission failure", Ydb.StatusIds_UNAUTHORIZED, true, 7, true},
		{"not released", Ydb.StatusIds_SUCCESS, false, 7, true},
		{"wrong stopped session", Ydb.StatusIds_SUCCESS, true, 8, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			sends := 0
			s := testSession(c, func(request *wire.SessionRequest) *wire.SessionResponse {
				sends++
				if release := request.GetReleaseSemaphore(); release != nil {
					return &wire.SessionResponse{Response: &wire.SessionResponse_ReleaseSemaphoreResult_{ReleaseSemaphoreResult: &wire.SessionResponse_ReleaseSemaphoreResult{ReqId: release.ReqId, Status: test.status, Released: test.released}}}
				}
				return &wire.SessionResponse{Response: &wire.SessionResponse_SessionStopped_{SessionStopped: &wire.SessionResponse_SessionStopped{SessionId: test.stopID}}}
			})
			s.acquired = true
			s.name = "lock"
			for range 2 {
				err := s.Close(c.Context())
				c.Assert(err != nil, qt.Equals, test.wantError)
			}
			c.Assert(sends, qt.Equals, 2)
		})
	}
}
func TestOwnershipRequiresSuccessfulExactResponse(t *testing.T) {
	for _, name := range []string{"status", "identity", "missing"} {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			s := testSession(c, func(request *wire.SessionRequest) *wire.SessionResponse {
				result := &wire.SessionResponse_DescribeSemaphoreResult{ReqId: request.GetDescribeSemaphore().ReqId, Status: Ydb.StatusIds_SUCCESS, SemaphoreDescription: &wire.SemaphoreDescription{Name: "lock"}}
				switch name {
				case "status":
					result.Status = Ydb.StatusIds_UNAUTHORIZED
				case "identity":
					result.ReqId++
				case "missing":
					result.SemaphoreDescription = nil
				}
				return &wire.SessionResponse{Response: &wire.SessionResponse_DescribeSemaphoreResult_{DescribeSemaphoreResult: result}}
			})
			value, err := s.DescribeSemaphore(c.Context(), "lock")
			c.Assert(err, qt.IsNotNil)
			c.Assert(value, qt.IsNil)
		})
	}
}
