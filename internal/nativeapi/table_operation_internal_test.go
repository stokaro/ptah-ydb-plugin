// White-box testing required: Table holds the native service clients in
// unexported fields, and only a Table built around fake clients can answer an
// AlterTable call with a running operation and then read it. No exported
// constructor takes a client, and a live server finishes an index build too
// fast to show the wait.

package nativeapi

import (
	"context"
	"errors"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"
)

type fakeAlter struct {
	Ydb_Table_V1.TableServiceClient
	reply *Ydb_Operations.Operation
	err   error
}

func (f fakeAlter) AlterTable(context.Context, *Ydb_Table.AlterTableRequest, ...grpc.CallOption) (*Ydb_Table.AlterTableResponse, error) {
	return &Ydb_Table.AlterTableResponse{Operation: f.reply}, f.err
}

// fakeOperations answers each read with the next reply and repeats the last one.
type fakeOperations struct {
	Ydb_Operation_V1.OperationServiceClient
	replies []*Ydb_Operations.Operation
	err     error
	reads   *[]string
}

func (f fakeOperations) GetOperation(_ context.Context, in *Ydb_Operations.GetOperationRequest, _ ...grpc.CallOption) (*Ydb_Operations.GetOperationResponse, error) {
	*f.reads = append(*f.reads, in.Id)
	reply := f.replies[min(len(*f.reads)-1, len(f.replies)-1)]
	return &Ydb_Operations.GetOperationResponse{Operation: reply}, f.err
}

func running(id string) *Ydb_Operations.Operation { return &Ydb_Operations.Operation{Id: id} }
func finished(status Ydb.StatusIds_StatusCode) *Ydb_Operations.Operation {
	return &Ydb_Operations.Operation{Id: "build", Ready: true, Status: status}
}

var (
	addIndex  = &Ydb_Table.AlterTableRequest{AddIndexes: []*Ydb_Table.TableIndex{{Name: "by_label"}}}
	dropIndex = &Ydb_Table.AlterTableRequest{DropIndexes: []string{"by_label"}}
)

func table(alter fakeAlter, operations fakeOperations) *Table {
	return &Table{client: alter, operations: operations, id: "session", poll: time.Millisecond}
}

func TestTableAlter_IndexBuildHappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		request   *Ydb_Table.AlterTableRequest
		reply     *Ydb_Operations.Operation
		replies   []*Ydb_Operations.Operation
		wantReads []string
	}{
		{"a finished reply is not read again", addIndex, finished(Ydb.StatusIds_SUCCESS), nil, nil},
		{"a running build is read until it succeeds", addIndex, running("build"),
			[]*Ydb_Operations.Operation{running("build"), running("build"), finished(Ydb.StatusIds_SUCCESS)}, []string{"build", "build", "build"}},
		{"a running build that finishes at the first read", addIndex, running("build"),
			[]*Ydb_Operations.Operation{finished(Ydb.StatusIds_SUCCESS)}, []string{"build"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			var reads []string

			err := table(fakeAlter{reply: test.reply}, fakeOperations{replies: test.replies, reads: &reads}).Alter(c.Context(), test.request)

			c.Assert(err, qt.IsNil)
			c.Assert(reads, qt.DeepEquals, test.wantReads)
		})
	}
}

func TestTableAlter_IndexBuildFailurePath(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name    string
		ctx     context.Context
		request *Ydb_Table.AlterTableRequest
		alter   fakeAlter
		replies []*Ydb_Operations.Operation
		opErr   error
		message string
	}{
		{"the build fails", context.Background(), addIndex, fakeAlter{reply: running("build")},
			[]*Ydb_Operations.Operation{running("build"), finished(Ydb.StatusIds_PRECONDITION_FAILED)},
			nil, "YDB operation failed: ready=true status=PRECONDITION_FAILED"},
		{"the running reply names no operation", context.Background(), addIndex, fakeAlter{reply: &Ydb_Operations.Operation{}}, nil, nil,
			"YDB operation failed: ready=false status=STATUS_CODE_UNSPECIFIED"},
		{"another alteration must finish in its reply", context.Background(), dropIndex, fakeAlter{reply: running("build")}, nil, nil,
			"YDB operation failed: ready=false status=STATUS_CODE_UNSPECIFIED"},
		{"the operation cannot be read", context.Background(), addIndex, fakeAlter{reply: running("build")},
			[]*Ydb_Operations.Operation{running("build")}, errors.New("transport"), "read YDB operation: transport failed"},
		{"the call is lost", context.Background(), addIndex, fakeAlter{err: errors.New("transport")}, nil, nil, "alter YDB table: transport failed"},
		{"the wait is canceled", canceled, addIndex, fakeAlter{reply: running("build")}, []*Ydb_Operations.Operation{running("build")}, nil, "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			var reads []string

			err := table(test.alter, fakeOperations{replies: test.replies, err: test.opErr, reads: &reads}).Alter(test.ctx, test.request)

			c.Assert(err, qt.ErrorMatches, test.message)
		})
	}
}
