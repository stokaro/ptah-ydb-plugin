package metadata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// Snapshot owns a private immutable spool. Memory is bounded by a record and
// an RPC page, not by journal length. Close releases its handle and file.
type Snapshot struct {
	store     Store
	file      *os.File
	remaining uint64
	sealed    bool
	closeErr  error
}

// NewSnapshot allocates a private file whose lifetime follows its handle.
// Unix unlinks it immediately; Windows requests native deletion on close.
func NewSnapshot(store Store) (*Snapshot, error) {
	file, err := createPrivateSpool()
	if err != nil {
		return nil, err
	}
	return &Snapshot{store: store, file: file}, nil
}
func (s *Snapshot) append(payload []byte) error {
	if s.file == nil || s.sealed || len(payload) == 0 || len(payload) > provider.MaxMetadataBytes {
		return fmt.Errorf("invalid YDB snapshot append")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := s.file.Write(header[:]); err != nil {
		return fmt.Errorf("write YDB metadata snapshot failed")
	}
	if _, err := s.file.Write(payload); err != nil {
		return fmt.Errorf("write YDB metadata snapshot failed")
	}
	return nil
}
func (s *Snapshot) seal(count uint64) error {
	if s.file == nil || s.sealed {
		return fmt.Errorf("YDB snapshot is not writable")
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind YDB metadata snapshot failed")
	}
	s.remaining = count
	s.sealed = true
	return nil
}

// Next consumes one complete record, or returns io.EOF after verified completion.
func (s *Snapshot) Next() (proto.Message, error) {
	if s.file == nil || !s.sealed {
		return nil, fmt.Errorf("YDB metadata snapshot is closed")
	}
	if s.remaining == 0 {
		var extra [1]byte
		n, err := s.file.Read(extra[:])
		if n != 0 || err != io.EOF {
			return nil, fmt.Errorf("YDB metadata snapshot has trailing data")
		}
		return nil, io.EOF
	}
	var header [4]byte
	if _, err := io.ReadFull(s.file, header[:]); err != nil {
		return nil, fmt.Errorf("YDB metadata snapshot is incomplete")
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > provider.MaxMetadataBytes {
		return nil, fmt.Errorf("invalid YDB metadata snapshot record size")
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(s.file, payload); err != nil {
		return nil, fmt.Errorf("YDB metadata snapshot is incomplete")
	}
	row, err := s.store.Decode(payload)
	if err != nil {
		return nil, err
	}
	s.remaining--
	return row, nil
}

// Close retains cleanup errors, so repetition cannot convert failure to success.
func (s *Snapshot) Close() error {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("close YDB metadata snapshot failed"))
		}
		s.file = nil
	}
	return s.closeErr
}
