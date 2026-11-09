package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
)

// serialOwner is app.records, which these tests create and change.
func serialOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
}

// serialSequence is the sequence YDB reports for a BigSerial id that steps by
// increment from start, with the data type YDB reports beside it.
func serialSequence(start, increment int64) *native.SequenceDescription {
	return &native.SequenceDescription{Name: new("_serial_column_id"), MinValue: proto.Int64(1), MaxValue: proto.Int64(9223372036854775807),
		StartValue: new(start), Cache: proto.Uint64(1), Increment: new(increment), Cycle: new(false),
		DataType: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT64}}}
}

// serialRecords is the read of app.records whose key id is a BigSerial with
// the sequence given.
func serialRecords(sequence *native.SequenceDescription) *native.DescribeTableResult {
	return &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE}, PrimaryKey: []string{"id"},
		Columns: []*native.ColumnMeta{{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT64}}, NotNull: new(true),
			DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: sequence}}}}
}

// serialCreation answers a creation of app.records: an empty directory, and
// the table read back as observed.
type serialCreation struct {
	Ydb_Scheme_V1.SchemeServiceClient
	observed *native.DescribeTableResult
}

func (f *serialCreation) ListDirectory(context.Context, *Ydb_Scheme.ListDirectoryRequest, ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	return &Ydb_Scheme.ListDirectoryResponse{Operation: result(&Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}})}, nil
}
func (f *serialCreation) Create(context.Context, *Ydb_Table.CreateTableRequest) error { return nil }
func (f *serialCreation) Describe(context.Context, string) (*native.DescribeTableResult, error) {
	return proto.CloneOf(f.observed), nil
}
func (f *serialCreation) Close() error { return nil }

// applySerialCreation creates app.records with a BigSerial id that steps by 5
// from 100, against a server that reads the table back as observed.
func applySerialCreation(c *qt.C, observed *native.DescribeTableResult) (bool, error) {
	c.Helper()
	fixture := &serialCreation{observed: observed}
	api := &nativeapi.TableCreation{Scheme: fixture, Open: func(context.Context) (nativeapi.CreationSession, error) { return fixture, nil }}
	operation, err := tablecreate.Capture("create", serialOwner(), &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "BIGSERIAL", Primary: true, IdentityStart: "100", IdentityIncrement: "5"}}},
		map[string]bool{"serial_columns": true, "serial_sequence_options": true})
	c.Assert(err, qt.IsNil)
	return api.Apply(c.Context(), "/local", operation.Value, func(context.Context) error { return nil })
}

// The created table is confirmed when YDB reads its sequence back as the
// request created it, with the data type YDB reports beside it.
func TestTableCreationConfirmsTheSerialSequence_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dispatched, err := applySerialCreation(c, serialRecords(serialSequence(100, 5)))
	c.Assert(err, qt.IsNil)
	c.Assert(dispatched, qt.IsTrue)
}

// A sequence that reads back otherwise than the request asked leaves the
// creation unconfirmed.
func TestTableCreationConfirmsTheSerialSequence_FailurePath(t *testing.T) {
	restarted := serialSequence(100, 5)
	restarted.SetVal = &native.SequenceDescription_SetVal{NextValue: proto.Int64(100)}
	narrowType := serialSequence(100, 5)
	narrowType.DataType = &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32}}
	widened := serialSequence(100, 5)
	widened.MaxValue = proto.Int64(2147483647)
	for _, test := range []struct {
		name     string
		sequence *native.SequenceDescription
	}{
		{name: "another increment", sequence: serialSequence(100, 3)},
		{name: "another start", sequence: serialSequence(1, 5)},
		{name: "a restart", sequence: restarted},
		{name: "a data type other than Int64", sequence: narrowType},
		{name: "another maximum", sequence: widened},
		{name: "no sequence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed := serialRecords(test.sequence)
			withSequence(observed.Columns[0], test.sequence)
			dispatched, err := applySerialCreation(c, observed)
			c.Assert(err, qt.ErrorMatches, `YDB created column differs from the request`)
			c.Assert(dispatched, qt.IsTrue)
		})
	}
}

// withSequence gives column a default from sequence, or no default for a nil
// sequence.
func withSequence(column *native.ColumnMeta, sequence *native.SequenceDescription) {
	column.DefaultValue = nil
	if sequence != nil {
		column.DefaultValue = &native.ColumnMeta_FromSequence{FromSequence: sequence}
	}
}

// sequenceSession reads app.records once before the change and once after.
type sequenceSession struct {
	before, after *native.DescribeTableResult
	reads         int
}

func (s *sequenceSession) Describe(context.Context, string) (*native.DescribeTableResult, error) {
	s.reads++
	if s.reads == 1 {
		return proto.CloneOf(s.before), nil
	}
	return proto.CloneOf(s.after), nil
}
func (s *sequenceSession) Close() error { return nil }

// sequenceRun is one Apply of the change of app.records' id sequence from
// 100 by 5 to 100 by 7, against the reads given.
type sequenceRun struct {
	before, after *native.DescribeTableResult
	executeErr    error
	database      string
	capabilities  map[string]bool
	held          *native.ColumnMeta
}

// applySequence runs the change and returns what Apply answered and the
// statements it sent.
func applySequence(c *qt.C, run sequenceRun) (bool, []string, error) {
	c.Helper()
	held := run.held
	if held == nil {
		held = proto.CloneOf(serialRecords(serialSequence(100, 5)).Columns[0])
	}
	operation, err := tablecreate.CaptureSequence("sequence", serialOwner(), &tablecreate.SequenceAlteration{Held: held, Settings: ydbsequence.Settings{Start: 100, Increment: 7}})
	c.Assert(err, qt.IsNil)
	session := &sequenceSession{before: run.before, after: run.after}
	var statements []string
	api := &nativeapi.SerialSequence{Open: func(context.Context) (nativeapi.DefaultSession, error) { return session, nil },
		Execute: func(_ context.Context, statement string) error {
			statements = append(statements, statement)
			return run.executeErr
		}}
	database := run.database
	if database == "" {
		database = "/local"
	}
	dispatched, err := api.Apply(c.Context(), database, operation.Value, run.capabilities, func(context.Context) error { return nil })
	return dispatched, statements, err
}

// The change sends one ALTER SEQUENCE on the sequence's absolute path, without
// a restart, and is confirmed by the sequence read back.
func TestSerialSequenceApply_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dispatched, statements, err := applySequence(c, sequenceRun{before: serialRecords(serialSequence(100, 5)), after: serialRecords(serialSequence(100, 7))})
	c.Assert(err, qt.IsNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(statements, qt.DeepEquals, []string{"ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 100 INCREMENT BY 7;"})
}

// A table that changed since the plan, and a narrow Serial on a target that
// does not keep its range, are refused before anything is sent.
func TestSerialSequenceApply_RefusedBeforeDispatch(t *testing.T) {
	narrow := serialRecords(serialSequence(100, 5))
	narrow.Columns[0].Type = &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32}}
	restarted := serialSequence(100, 5)
	restarted.SetVal = &native.SequenceDescription_SetVal{NextValue: proto.Int64(110)}
	for _, test := range []struct {
		name string
		run  sequenceRun
		want string
	}{
		{name: "another increment since the plan", run: sequenceRun{before: serialRecords(serialSequence(100, 6))}, want: `YDB sequence before-state differs from the plan`},
		{name: "a restart since the plan", run: sequenceRun{before: serialRecords(restarted)}, want: `YDB sequence before-state differs from the plan`},
		{name: "a narrow Serial", run: sequenceRun{before: narrow, held: proto.CloneOf(narrow.Columns[0])},
			want: `YDB raises the maximum of a Serial's sequence on any ALTER SEQUENCE, and this target does not keep it`},
		{name: "another database", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), database: "/other"}, want: `YDB sequence change targets another database`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dispatched, statements, err := applySequence(c, test.run)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(statements, qt.HasLen, 0)
		})
	}
}

// A reply that failed, or a sequence that reads back otherwise than asked,
// leaves the change unconfirmed after it was sent.
func TestSerialSequenceApply_Unconfirmed(t *testing.T) {
	restarted := serialSequence(100, 7)
	restarted.SetVal = &native.SequenceDescription_SetVal{NextValue: proto.Int64(100)}
	cached := serialSequence(100, 7)
	cached.Cache = proto.Uint64(10)
	moved := serialRecords(serialSequence(100, 7))
	moved.Columns = append(moved.Columns, &native.ColumnMeta{Name: "note", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}})
	for _, test := range []struct {
		name string
		run  sequenceRun
		want string
	}{
		{name: "a lost reply", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), executeErr: errors.New("lost reply")}, want: `lost reply`},
		{name: "the increment not taken", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), after: serialRecords(serialSequence(100, 5))},
			want: `YDB sequence differs from the request after the change`},
		{name: "a restart", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), after: serialRecords(restarted)},
			want: `YDB sequence differs from the request after the change`},
		{name: "another setting", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), after: serialRecords(cached)},
			want: `YDB sequence changed outside the requested start and increment`},
		{name: "another column", run: sequenceRun{before: serialRecords(serialSequence(100, 5)), after: moved},
			want: `YDB column inventory or key changed during the sequence change`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dispatched, statements, err := applySequence(c, test.run)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(statements, qt.HasLen, 1)
		})
	}
}
