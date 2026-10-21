package backend

// Reader state is private: these tests verify fencing and resource state even
// when no database call is needed for an absent or already materialized store.
import (
	"context"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func readerFixture() (*Provider, *session, *protocol.MetadataReadRequest) {
	p := New()
	s := &session{database: "/local", metadataReaders: make(map[string]*metadataReader)}
	p.sessions["s"] = s
	request := &protocol.MetadataReadRequest{Scope: &protocol.MetadataScope{SessionId: "s", Schema: "app", Table: "tags", Kind: protocol.MetadataScope_TAGS}, RequestId: "request", ReaderId: "reader"}
	return p, s, request
}
func TestMetadataReaderCloseFencesOpenAndRetainsScope(t *testing.T) {
	c := qt.New(t)
	p, s, request := readerFixture()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.CloseMetadataRead(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(s.metadataReaders[request.ReaderId].closed, qt.IsTrue)
	_, err = p.OpenMetadataRead(t.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.AlreadyExists)
	wrong := proto.CloneOf(request)
	wrong.Scope.Table = "other"
	_, err = p.CloseMetadataRead(t.Context(), wrong)
	c.Assert(err, qt.IsNotNil)
	_, err = p.CloseMetadataRead(t.Context(), request)
	c.Assert(err, qt.IsNil)
}
func TestMetadataPageFailureClosesSnapshot(t *testing.T) {
	c := qt.New(t)
	p, s, request := readerFixture()
	store, err := metadata.Resolve("/local", request.Scope)
	c.Assert(err, qt.IsNil)
	snapshot, err := metadata.NewSnapshot(store)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(snapshot.Close(), qt.IsNil) })
	record := &metadataReader{scope: proto.CloneOf(request.Scope), snapshot: snapshot, exists: true}
	s.metadataReaders[request.ReaderId] = record
	// An unsealed spool is a controlled read failure, not a complete empty store.
	result, err := p.ReadMetadataPage(t.Context(), &protocol.ReadMetadataPageRequest{Reader: request, MaxRecords: 1})
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
	c.Assert(record.closed, qt.IsTrue)
	c.Assert(record.snapshot, qt.IsNil)
	_, err = p.CloseMetadataRead(t.Context(), request)
	c.Assert(err, qt.IsNil)
}
func TestMetadataReaderBoundsAndTransactionCleanup(t *testing.T) {
	c := qt.New(t)
	p, s, request := readerFixture()
	for i := 0; i < metadataOpenLimit; i++ {
		s.metadataReaders[strconv.Itoa(i)] = &metadataReader{scope: proto.CloneOf(request.Scope)}
	}
	_, err := p.OpenMetadataRead(t.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.ResourceExhausted)
	c.Assert(s.metadataReaders[request.ReaderId].closed, qt.IsTrue)
	s.closeMetadataReaders("", true)
	for _, reader := range s.metadataReaders {
		c.Assert(reader.closed, qt.IsTrue)
	}
	s.metadataReaders = make(map[string]*metadataReader)
	for i := 0; i < metadataReaderLimit; i++ {
		s.metadataReaders[strconv.Itoa(i)] = &metadataReader{scope: proto.CloneOf(request.Scope), closed: true}
	}
	_, err = p.CloseMetadataRead(t.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.ResourceExhausted)
	existing := proto.CloneOf(request)
	existing.ReaderId = "0"
	_, err = p.CloseMetadataRead(t.Context(), existing)
	c.Assert(err, qt.IsNil)
}
func TestMetadataSnapshotScopeCannotCloseAnotherTransaction(t *testing.T) {
	c := qt.New(t)
	_, s, request := readerFixture()
	bound := proto.CloneOf(request.Scope)
	bound.TransactionId = "tx"
	selected := &metadataReader{scope: bound}
	other := &metadataReader{scope: proto.CloneOf(request.Scope)}
	s.metadataReaders["selected"] = selected
	s.metadataReaders["other"] = other
	s.closeMetadataReaders("tx", false)
	c.Assert(selected.closed, qt.IsTrue)
	c.Assert(other.closed, qt.IsFalse)
	s.closeMetadataReaders("", true)
	c.Assert(other.closed, qt.IsTrue)
}
