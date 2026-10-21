package metadata

// These tests inspect the spool handle because pathname checks cannot prove
// release of an unlinked Unix file or a retained Windows handle.
import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func testStore(c *qt.C, kind protocol.MetadataScope_Kind) Store {
	c.Helper()
	s, err := Resolve("/local", &protocol.MetadataScope{SessionId: "s", Schema: "app", Table: "store", Kind: kind})
	c.Assert(err, qt.IsNil)
	return s
}
func event() *protocol.MigrationLogRecord {
	return &protocol.MigrationLogRecord{RunId: "run π", Sequence: 9223372036854775807, Operation: "down", Version: -9223372036854775808, State: "failed", Actor: "actor\x00π", ActorSource: "provided", Checksum: "hash\x00", Error: "error\n\x00", AtSeconds: -62135596800, AtNanos: 999999999}
}
func tag() *protocol.MigrationTagRecord {
	return &protocol.MigrationTagRecord{Name: "exact π", Version: -9223372036854775808, RecordedAtSeconds: 253402300799, RecordedAtNanos: 999999999}
}
func TestMetadataCodecPreservesFactsAndRefusesUnknownFormats(t *testing.T) {
	c := qt.New(t)
	for _, test := range []struct {
		kind protocol.MetadataScope_Kind
		row  proto.Message
	}{{protocol.MetadataScope_LOG, event()}, {protocol.MetadataScope_TAGS, tag()}} {
		s := testStore(c, test.kind)
		payload, err := s.Encode(test.row)
		c.Assert(err, qt.IsNil)
		decoded, err := s.Decode(payload)
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(decoded, test.row), qt.IsTrue)
		for _, bad := range [][]byte{[]byte("other"), protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), payload...), 99, protowire.VarintType), 1), protowire.AppendString(protowire.AppendTag(append([]byte(nil), payload...), 1, protowire.BytesType), "duplicate")} {
			value, err := s.Decode(bad)
			c.Assert(err, qt.IsNotNil)
			c.Assert(value, qt.IsNil)
		}
		snapshot, err := NewSnapshot(s)
		c.Assert(err, qt.IsNil)
		c.Cleanup(func() { c.Check(snapshot.Close(), qt.IsNil) })
		c.Assert(snapshot.append(payload), qt.IsNil)
		c.Assert(snapshot.seal(1), qt.IsNil)
		read, err := snapshot.Next()
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(test.row, read), qt.IsTrue)
		_, err = snapshot.Next()
		c.Assert(err, qt.ErrorIs, io.EOF)
		filename := snapshot.file.Name()
		c.Assert(snapshot.Close(), qt.IsNil)
		_, statErr := os.Stat(filename)
		c.Assert(statErr, qt.ErrorIs, os.ErrNotExist)
		c.Assert(snapshot.file, qt.IsNil)
		c.Assert(snapshot.Close(), qt.IsNil)
	}
}
func TestMetadataSnapshotRefusesTruncatedAndTrailingData(t *testing.T) {
	for _, test := range []struct {
		name     string
		count    uint64
		truncate bool
	}{{"missing record", 2, false}, {"trailing record", 0, false}, {"short frame", 1, true}} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			s := testStore(c, protocol.MetadataScope_TAGS)
			payload, err := s.Encode(tag())
			c.Assert(err, qt.IsNil)
			snapshot, err := NewSnapshot(s)
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(snapshot.Close(), qt.IsNil) })
			c.Assert(snapshot.append(payload), qt.IsNil)
			if test.truncate {
				c.Assert(snapshot.file.Truncate(2), qt.IsNil)
			}
			c.Assert(snapshot.seal(test.count), qt.IsNil)
			_, err = snapshot.Next()
			if test.count == 2 {
				c.Assert(err, qt.IsNil)
				_, err = snapshot.Next()
			}
			c.Assert(err, qt.IsNotNil)
			c.Assert(err, qt.Not(qt.ErrorIs), io.EOF)
		})
	}
}

// Native read failures, truncated streams, and lost mutation replies cannot be
// injected deterministically through a live database; the SQL boundary can.
type storageProbe struct {
	count              int64
	rows, lookup       [][]driver.Value
	execErr, errorRead error
	writes             int
}
type probeConnector struct{ p *storageProbe }

func (c probeConnector) Connect(context.Context) (driver.Conn, error) { return &probeConn{p: c.p}, nil }
func (probeConnector) Driver() driver.Driver                          { return probeDriver{} }

type probeDriver struct{}

func (probeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type probeConn struct{ p *storageProbe }

func (*probeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*probeConn) Close() error                        { return nil }
func (*probeConn) Begin() (driver.Tx, error)           { return probeTx{}, nil }

type probeTx struct{}

func (probeTx) Commit() error   { return nil }
func (probeTx) Rollback() error { return nil }
func (c *probeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.p.writes++
	return driver.RowsAffected(0), c.p.execErr
}
func (c *probeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.p.errorRead != nil {
		return nil, c.p.errorRead
	}
	if strings.Contains(query, "COUNT(*)") {
		return &probeRows{columns: []string{"count"}, data: [][]driver.Value{{c.p.count}}}, nil
	}
	if strings.Contains(query, " WHERE ") {
		return &probeRows{columns: []string{"record"}, data: c.p.lookup}, nil
	}
	columns := []string{"name", "record"}
	if strings.Contains(query, "run_id") {
		columns = []string{"run_id", "sequence", "record"}
	}
	return &probeRows{columns: columns, data: c.p.rows}, nil
}

type probeRows struct {
	columns []string
	data    [][]driver.Value
	index   int
}

func (r *probeRows) Columns() []string { return r.columns }
func (*probeRows) Close() error        { return nil }
func (r *probeRows) Next(dst []driver.Value) error {
	if r.index == len(r.data) {
		return io.EOF
	}
	copy(dst, r.data[r.index])
	r.index++
	return nil
}
func transactionProbe(c *qt.C, p *storageProbe) *sql.Tx {
	c.Helper()
	db := sql.OpenDB(probeConnector{p: p})
	c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
	tx, err := db.BeginTx(c.Context(), nil)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			c.Check(err, qt.IsNil)
		}
	})
	return tx
}
func TestMetadataMutationPreservesUncertainAndAbsentOutcomes(t *testing.T) {
	for _, test := range []struct {
		name                     string
		kind                     protocol.MetadataScope_Kind
		request                  *protocol.MutateMetadataRequest
		present, failWrite, deny bool
		outcome                  protocol.MetadataResult_Outcome
		deleted                  bool
		writes                   int
	}{
		{"duplicate log", protocol.MetadataScope_LOG, &protocol.MutateMetadataRequest{Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: event()}}, true, false, false, protocol.MetadataResult_REJECTED, false, 0},
		{"lost log reply", protocol.MetadataScope_LOG, &protocol.MutateMetadataRequest{Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: event()}}, false, true, false, protocol.MetadataResult_UNKNOWN, false, 1},
		{"lease refusal", protocol.MetadataScope_LOG, &protocol.MutateMetadataRequest{Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: event()}}, false, false, true, protocol.MetadataResult_REJECTED, false, 0},
		{"delete present", protocol.MetadataScope_TAGS, &protocol.MutateMetadataRequest{Mutation: &protocol.MutateMetadataRequest_DeleteTag{DeleteTag: tag().Name}}, true, false, false, protocol.MetadataResult_APPLIED, true, 1},
		{"delete absent", protocol.MetadataScope_TAGS, &protocol.MutateMetadataRequest{Mutation: &protocol.MutateMetadataRequest_DeleteTag{DeleteTag: tag().Name}}, false, false, false, protocol.MetadataResult_APPLIED, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			s := testStore(c, test.kind)
			p := &storageProbe{}
			if test.present {
				var row proto.Message = tag()
				if test.kind == protocol.MetadataScope_LOG {
					row = event()
				}
				payload, err := s.Encode(row)
				c.Assert(err, qt.IsNil)
				p.lookup = [][]driver.Value{{payload}}
			}
			if test.failWrite {
				p.execErr = driver.ErrBadConn
			}
			outcome, deleted, err := s.Mutate(t.Context(), transactionProbe(c, p), test.request, func(context.Context) error {
				if test.deny {
					return errors.New("lost lease")
				}
				return nil
			})
			if test.failWrite || test.deny {
				c.Assert(err, qt.IsNotNil)
			} else {
				c.Assert(err, qt.IsNil)
			}
			c.Assert(outcome, qt.Equals, test.outcome)
			c.Assert(deleted, qt.Equals, test.deleted)
			c.Assert(p.writes, qt.Equals, test.writes)
		})
	}
}
func TestMetadataSnapshotChecksCountKeysAndCompleteRecords(t *testing.T) {
	c := qt.New(t)
	s := testStore(c, protocol.MetadataScope_TAGS)
	payload, err := s.Encode(tag())
	c.Assert(err, qt.IsNil)
	for _, test := range []struct {
		name  string
		count int64
		rows  [][]driver.Value
		valid bool
	}{
		{"complete", 1, [][]driver.Value{{tag().Name, payload}}, true}, {"empty", 0, nil, true}, {"truncated", 2, [][]driver.Value{{tag().Name, payload}}, false}, {"wrong key", 1, [][]driver.Value{{"other", payload}}, false}, {"duplicate", 2, [][]driver.Value{{tag().Name, payload}, {tag().Name, payload}}, false}, {"corrupt", 1, [][]driver.Value{{tag().Name, []byte("bad")}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			snapshot, err := s.Snapshot(t.Context(), transactionProbe(c, &storageProbe{count: test.count, rows: test.rows}))
			if !test.valid {
				c.Assert(err, qt.IsNotNil)
				c.Assert(snapshot, qt.IsNil)
				return
			}
			c.Assert(err, qt.IsNil)
			c.Assert(snapshot.Close(), qt.IsNil)
		})
	}
}
