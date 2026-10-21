package locking

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	wire "github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination/options"
)

// wireSession uses one stream without reconnect or acquisition replay. Every
// operation validates its native status; an absent result is never success.
// Stop acknowledgment, rather than local stream shutdown, confirms cleanup.
type wireSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	stream    Ydb_Coordination_V1.CoordinationService_SessionClient
	responses chan *wire.SessionResponse
	receiving chan struct{}
	sendMu    sync.Mutex
	callMu    sync.Mutex
	id        uint64
	requestID uint64
	name      string
	acquired  bool
	ended     atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func openSession(ctx context.Context, driver *ydb.Driver, node string) (*wireSession, error) {
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	stream, err := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(driver)).Session(lifetime)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open YDB coordination stream failed")
	}
	s := &wireSession{ctx: lifetime, cancel: cancel, stream: stream, responses: make(chan *wire.SessionResponse, 8), receiving: make(chan struct{})}
	go s.receive()
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	result, err := s.call(ctx, &wire.SessionRequest{Request: &wire.SessionRequest_SessionStart_{SessionStart: &wire.SessionRequest_SessionStart{Path: node, TimeoutMillis: uint64(sessionTimeout / time.Millisecond), Description: "ptah-ydb-plugin", ProtectionKey: key}}})
	if err != nil || result.GetSessionStarted().GetSessionId() == 0 {
		cancel()
		<-s.receiving
		return nil, fmt.Errorf("start YDB lock session failed")
	}
	s.id = result.GetSessionStarted().GetSessionId()
	if !stop() || ctx.Err() != nil {
		cancel()
		<-s.receiving
		return nil, fmt.Errorf("start YDB lock session canceled")
	}
	go s.keepAlive()
	return s, nil
}
func (s *wireSession) Context() context.Context { return s.ctx }
func (s *wireSession) SessionID() uint64        { return s.id }
func (s *wireSession) send(request *wire.SessionRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ctx.Err() != nil {
		return fmt.Errorf("YDB lock stream ended")
	}
	if err := s.stream.Send(request); err != nil {
		s.cancel()
		return fmt.Errorf("send YDB lock request failed")
	}
	return nil
}
func (s *wireSession) receive() {
	defer close(s.receiving)
	defer s.cancel()
	for {
		response, err := s.stream.Recv()
		if err != nil {
			return
		}
		if schema.Known(response) != nil {
			return
		}
		switch value := response.Response.(type) {
		case *wire.SessionResponse_Ping:
			if s.send(&wire.SessionRequest{Request: &wire.SessionRequest_Pong{Pong: &wire.SessionRequest_PingPong{Opaque: value.Ping.GetOpaque()}}}) != nil {
				return
			}
		case *wire.SessionResponse_Pong:
		case *wire.SessionResponse_Failure_:
			if value.Failure.GetStatus() == Ydb.StatusIds_SESSION_EXPIRED {
				s.ended.Store(true)
			}
			return
		default:
			select {
			case s.responses <- response:
			case <-s.ctx.Done():
				return
			}
		}
	}
}
func (s *wireSession) keepAlive() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.send(&wire.SessionRequest{Request: &wire.SessionRequest_Ping{Ping: &wire.SessionRequest_PingPong{Opaque: 1}}}) != nil {
				return
			}
		}
	}
}
func (s *wireSession) next(ctx context.Context) (*wire.SessionResponse, error) {
	// Preserve an acknowledgment queued just before the server closes its stream.
	select {
	case r := <-s.responses:
		return r, nil
	default:
	}
	select {
	case r := <-s.responses:
		return r, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("YDB lock request canceled")
	case <-s.ctx.Done():
		select {
		case r := <-s.responses:
			return r, nil
		default:
			return nil, fmt.Errorf("YDB lock stream ended")
		}
	}
}
func (s *wireSession) call(ctx context.Context, request *wire.SessionRequest) (*wire.SessionResponse, error) {
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	if err := s.send(request); err != nil {
		return nil, err
	}
	for {
		response, err := s.next(ctx)
		if err != nil {
			return nil, err
		}
		if pending := response.GetAcquireSemaphorePending(); pending != nil {
			if request.GetAcquireSemaphore() == nil || pending.ReqId != request.GetAcquireSemaphore().ReqId {
				s.cancel()
				return nil, fmt.Errorf("mismatched YDB lock pending response")
			}
			continue
		}
		return response, nil
	}
}
func (s *wireSession) acquire(ctx context.Context, name string) (bool, error) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	s.requestID++
	id := s.requestID
	response, err := s.call(ctx, &wire.SessionRequest{Request: &wire.SessionRequest_AcquireSemaphore_{AcquireSemaphore: &wire.SessionRequest_AcquireSemaphore{ReqId: id, Name: name, Count: coordination.Exclusive, Ephemeral: true, TimeoutMillis: 0}}})
	if err != nil {
		return false, err
	}
	result := response.GetAcquireSemaphoreResult()
	if result == nil || result.ReqId != id || result.Status != Ydb.StatusIds_SUCCESS {
		return false, fmt.Errorf("YDB lock acquisition was not confirmed")
	}
	s.name = name
	s.acquired = result.Acquired
	return result.Acquired, nil
}
func (s *wireSession) DescribeSemaphore(ctx context.Context, name string, _ ...options.DescribeSemaphoreOption) (*coordination.SemaphoreDescription, error) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	s.requestID++
	id := s.requestID
	response, err := s.call(ctx, &wire.SessionRequest{Request: &wire.SessionRequest_DescribeSemaphore_{DescribeSemaphore: &wire.SessionRequest_DescribeSemaphore{ReqId: id, Name: name, IncludeOwners: true}}})
	if err != nil {
		return nil, err
	}
	result := response.GetDescribeSemaphoreResult()
	if result == nil || result.ReqId != id || result.Status != Ydb.StatusIds_SUCCESS || result.SemaphoreDescription == nil {
		return nil, fmt.Errorf("YDB lock ownership response is invalid")
	}
	value := result.SemaphoreDescription
	described := &coordination.SemaphoreDescription{Name: value.Name, Count: value.Count, Limit: value.Limit, Ephemeral: value.Ephemeral}
	for _, owner := range value.Owners {
		described.Owners = append(described.Owners, &coordination.SemaphoreSession{SessionID: owner.GetSessionId(), Count: owner.GetCount()})
	}
	return described, nil
}
func (s *wireSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		defer s.cancel()
		s.callMu.Lock()
		defer s.callMu.Unlock()
		if s.ended.Load() {
			return
		}
		if s.acquired {
			s.requestID++
			id := s.requestID
			response, err := s.call(ctx, &wire.SessionRequest{Request: &wire.SessionRequest_ReleaseSemaphore_{ReleaseSemaphore: &wire.SessionRequest_ReleaseSemaphore{ReqId: id, Name: s.name}}})
			result := response.GetReleaseSemaphoreResult()
			if err != nil || result == nil || result.ReqId != id || result.Status != Ydb.StatusIds_SUCCESS || !result.Released {
				s.closeErr = fmt.Errorf("YDB semaphore release was not confirmed")
			}
		}
		response, err := s.call(ctx, &wire.SessionRequest{Request: &wire.SessionRequest_SessionStop_{SessionStop: &wire.SessionRequest_SessionStop{}}})
		if err != nil || response.GetSessionStopped().GetSessionId() != s.id {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("YDB lock session stop was not confirmed"))
		} else {
			s.ended.Store(true)
		}
	})
	return s.closeErr
}
