package backend

import (
	"context"
	"errors"
	"io"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
)

const metadataReaderLimit = 1024
const metadataOpenLimit = 16

type metadataReader struct {
	scope                         *protocol.MetadataScope
	snapshot                      *metadata.Snapshot
	pending                       proto.Message
	exists, closed, complete, eof bool
	page                          uint64
	closeErr                      error
}

func (r *metadataReader) close() error {
	r.closed = true
	r.pending = nil
	if r.snapshot != nil {
		r.closeErr = errors.Join(r.closeErr, r.snapshot.Close())
		r.snapshot = nil
	}
	return r.closeErr
}
func metadataReadResult(request *protocol.MetadataReadRequest) *protocol.MetadataReadResult {
	return &protocol.MetadataReadResult{Scope: proto.CloneOf(request.Scope), ReaderId: request.ReaderId, RequestId: request.RequestId}
}
func (s *session) metadataReservation(request *protocol.MetadataReadRequest) (*metadataReader, error) {
	reader := s.metadataReaders[request.ReaderId]
	if reader != nil && !proto.Equal(reader.scope, request.Scope) {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB metadata reader scope changed")
	}
	return reader, nil
}
func (s *session) reserveMetadata(request *protocol.MetadataReadRequest) (*metadataReader, error) {
	if len(s.metadataReaders) >= metadataReaderLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "YDB metadata reader handle limit reached; open another session")
	}
	if s.metadataReaders == nil {
		s.metadataReaders = make(map[string]*metadataReader)
	}
	reader := &metadataReader{scope: proto.CloneOf(request.Scope), closed: true}
	s.metadataReaders[request.ReaderId] = reader
	return reader, nil
}

// OpenMetadataRead captures the complete immutable snapshot before returning.
// A failed or lost open consumes its handle; Close still releases or fences it.
func (p *Provider) OpenMetadataRead(ctx context.Context, request *protocol.MetadataReadRequest) (result *protocol.MetadataReadResult, err error) {
	if err := provider.ValidateMetadataReadRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, store, err := p.metadataSession(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	previous, err := s.metadataReservation(request)
	if err != nil {
		return nil, err
	}
	if previous != nil {
		return nil, runtime.ErrorWithReason(codes.AlreadyExists, "YDB metadata reader handle was already reserved")
	}
	reader, err := s.reserveMetadata(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup := reader.close()
			s.cleanupErr = errors.Join(s.cleanupErr, cleanup)
			err = metadataFailure(s, request.Scope, errors.Join(err, cleanup))
			result = nil
		}
	}()
	active := 0
	for _, other := range s.metadataReaders {
		if !other.closed && !other.complete {
			active++
		}
	}
	if active >= metadataOpenLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "YDB metadata open reader limit reached")
	}
	store, reader.exists, err = store.Locate(ctx, s.connection.Driver)
	if err != nil {
		return nil, err
	}
	if reader.exists {
		work, err := s.openMetadataWork(ctx, request.Scope)
		if err != nil {
			return nil, err
		}
		reader.snapshot, err = store.Snapshot(ctx, work.runner)
		cleanup := work.close()
		s.cleanupErr = errors.Join(s.cleanupErr, cleanup)
		if err = errors.Join(err, cleanup); err != nil {
			return nil, err
		}
	} else {
		reader.eof = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.closed = false
	return metadataReadResult(request), nil
}

func (r *metadataReader) fill() error {
	if r.pending != nil || r.eof {
		return nil
	}
	row, err := r.snapshot.Next()
	if errors.Is(err, io.EOF) {
		r.eof = true
		return nil
	}
	if err != nil {
		return err
	}
	r.pending = row
	return nil
}

// ReadMetadataPage advances a cursor once. Failed pages close the snapshot;
// callers cannot replay an index after an uncertain reply.
func (p *Provider) ReadMetadataPage(ctx context.Context, request *protocol.ReadMetadataPageRequest) (result *protocol.MetadataPage, err error) {
	if err := provider.ValidateReadMetadataPageRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, _, err := p.metadataSession(ctx, request.Reader.Scope)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	reader, err := s.metadataReservation(request.Reader)
	if err != nil {
		return nil, err
	}
	if reader != nil && !reader.closed && !reader.complete && request.Page != reader.page {
		s.cleanupErr = errors.Join(s.cleanupErr, reader.close())
	}
	if reader == nil || reader.closed || reader.complete || request.Page != reader.page {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB metadata reader is not at the requested page")
	}
	defer func() {
		if err != nil {
			cleanup := reader.close()
			s.cleanupErr = errors.Join(s.cleanupErr, cleanup)
			result = nil
			err = metadataFailure(s, request.Reader.Scope, errors.Join(err, cleanup))
		}
	}()
	result = &protocol.MetadataPage{Reader: metadataReadResult(request.Reader), Page: request.Page, Exists: reader.exists}
	if err := reader.fillPage(ctx, result, request.MaxRecords); err != nil {
		return nil, err
	}
	if err := reader.fill(); err != nil {
		return nil, err
	}
	result.Complete = reader.eof
	if err := provider.ValidateMetadataPage(request, result); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reader.advance(result); err != nil {
		return nil, err
	}
	return result, nil
}

// fillPage appends up to limit records to page, in the snapshot's order. It
// stops early at the end of the snapshot, and before a record that would take
// the page past provider.MaxMetadataBytes, which stays pending for the next
// page; a page whose first record alone is too large is an error.
func (r *metadataReader) fillPage(ctx context.Context, page *protocol.MetadataPage, limit uint32) error {
	for count := range limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.fill(); err != nil {
			return err
		}
		if r.eof {
			return nil
		}
		switch row := r.pending.(type) {
		case *protocol.MigrationLogRecord:
			page.Logs = append(page.Logs, row)
		case *protocol.MigrationTagRecord:
			page.Tags = append(page.Tags, row)
		}
		if proto.Size(page) > provider.MaxMetadataBytes {
			if count == 0 {
				return runtime.ErrorWithReason(codes.ResourceExhausted, "YDB metadata record exceeds page size")
			}
			if len(page.Logs) > 0 {
				page.Logs = page.Logs[:len(page.Logs)-1]
			} else {
				page.Tags = page.Tags[:len(page.Tags)-1]
			}
			return nil
		}
		r.pending = nil
	}
	return nil
}

// advance moves the reader past the page it served. The page that completes
// the read closes its snapshot, and a failed close is the reader's error from
// then on.
func (r *metadataReader) advance(page *protocol.MetadataPage) error {
	r.page++
	r.complete = page.Complete
	if r.complete && r.snapshot != nil {
		r.closeErr = r.snapshot.Close()
		r.snapshot = nil
		if r.closeErr != nil {
			return r.closeErr
		}
	}
	return nil
}

// CloseMetadataRead releases resources even when the caller has canceled.
// A close preceding open fences that ID. A changed scope never closes a reader.
func (p *Provider) CloseMetadataRead(_ context.Context, request *protocol.MetadataReadRequest) (*protocol.MetadataReadResult, error) {
	if err := provider.ValidateMetadataReadRequest(request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	s, err := p.lookup(request.Scope.SessionId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, err := s.metadataReservation(request)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		if s.closed {
			return metadataReadResult(request), nil
		}
		reader, err = s.reserveMetadata(request)
		if err != nil {
			return nil, err
		}
	}
	if err := reader.close(); err != nil {
		s.cleanupErr = errors.Join(s.cleanupErr, err)
		return nil, runtime.ErrorWithReason(codes.Unavailable, "close YDB metadata reader failed")
	}
	return metadataReadResult(request), nil
}

// closeMetadataReaders closes every metadata reader of the session and keeps
// their cleanup errors on it.
func (s *session) closeMetadataReaders() {
	for _, reader := range s.metadataReaders {
		s.cleanupErr = errors.Join(s.cleanupErr, reader.close())
	}
}

// closeTransactionMetadataReaders closes the metadata readers bound to
// transactionID and keeps their cleanup errors on the session.
func (s *session) closeTransactionMetadataReaders(transactionID string) {
	for _, reader := range s.metadataReaders {
		if reader.scope.TransactionId == transactionID {
			s.cleanupErr = errors.Join(s.cleanupErr, reader.close())
		}
	}
}
