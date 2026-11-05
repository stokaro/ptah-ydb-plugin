package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
)

// describedPaths is a scheme service that describes the paths it holds and
// answers every other one with the status given. A call other than
// DescribePath reaches the embedded nil interface and panics.
type describedPaths struct {
	Ydb_Scheme_V1.SchemeServiceClient
	entries map[string]*Ydb_Scheme.Entry
	missing Ydb.StatusIds_StatusCode
}

func (s *describedPaths) DescribePath(_ context.Context, request *Ydb_Scheme.DescribePathRequest, _ ...grpc.CallOption) (*Ydb_Scheme.DescribePathResponse, error) {
	entry, found := s.entries[request.Path]
	if !found {
		return &Ydb_Scheme.DescribePathResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: s.missing}}, nil
	}
	return &Ydb_Scheme.DescribePathResponse{Operation: result(&Ydb_Scheme.DescribePathResult{Self: entry})}, nil
}

// unopened is a comment runner that fails the test if it opens a table
// session: a refusal before the write needs none.
func unopened(c *qt.C) func(context.Context) (nativeapi.AttributeSession, error) {
	return func(context.Context) (nativeapi.AttributeSession, error) {
		c.Fatalf("the statement opened a table session")
		return nil, errors.New("unreachable")
	}
}

// A comment on a path that holds nothing is refused before anything is
// written, naming the path, and the refusal is the statement's own: the
// statement was not dispatched. Any other failure to describe the path stays
// an error the provider does not word as a refusal.
func TestRunStatement_RefusesAPathThatHoldsNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		missing Ydb.StatusIds_StatusCode
		refused bool
		want    string
	}{
		{name: "a path YDB answers SCHEME_ERROR for", missing: Ydb.StatusIds_SCHEME_ERROR, refused: true,
			want: `invalid comment statement: set the comment on table /local/app/absent: YDB holds no object at the path, or the session may not see it \(SCHEME_ERROR\)`},
		{name: "a path whose description failed otherwise", missing: Ydb.StatusIds_UNAVAILABLE,
			want: `set the comment on table /local/app/absent: YDB operation failed: ready=true status=UNAVAILABLE`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize("COMMENT ON TABLE `app/absent` IS 'x'")
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			comments := &nativeapi.Comments{Scheme: &describedPaths{missing: test.missing}, Open: unopened(c)}

			dispatched, err := comments.RunStatement(c.Context(), "/local", "/local", query, func(context.Context) error { return nil })

			c.Assert(dispatched, qt.IsFalse)
			c.Assert(err, qt.ErrorMatches, test.want)
			_, isRefusal := errors.AsType[*nativeapi.Refusal](err)
			c.Assert(isRefusal, qt.Equals, test.refused)
			c.Assert(errors.Is(err, ydbcomment.ErrStatement), qt.Equals, test.refused)
		})
	}
}

// attributeSession is a table session over one table that answers the
// comment's write and the read after it with the operations given.
type attributeSession struct {
	alter, confirm *Ydb_Operations.Operation
	described      *native.DescribeTableResult
}

func (s *attributeSession) Describe(context.Context, string) (*native.DescribeTableResult, error) {
	if s.confirm != nil {
		return nil, nativeapi.Status(s.confirm)
	}
	return s.described, nil
}

func (s *attributeSession) Alter(context.Context, *Ydb_Table.AlterTableRequest) error {
	if err := nativeapi.Status(s.alter); err != nil {
		return err
	}
	s.described.Attributes = map[string]string{"ptah.comment": "rows"}
	return nil
}

func (*attributeSession) Close() error { return nil }

// The write of a comment is one AlterTable, which YDB applies whole or not
// at all, so a failure of it carries YDB's answer, its status and issues, for
// the reply to read: BAD_REQUEST for attributes past YDB's budget proves
// nothing was written. A failure of the read after a write that succeeded
// says nothing of the write, and carries no answer.
func TestRunStatement_CarriesTheAnswerToTheWrite(t *testing.T) {
	budget := []*Ydb_Issue.IssueMessage{{Message: "Error: user attributes too big: 10318"}}
	for _, test := range []struct {
		name           string
		alter, confirm *Ydb_Operations.Operation
		want           ydbfailure.Answer
		message        string
	}{
		{name: "a write YDB refused", alter: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_BAD_REQUEST, Issues: budget},
			want:    ydbfailure.Answer{Status: Ydb.StatusIds_BAD_REQUEST, Issues: []string{"Error: user attributes too big: 10318"}},
			message: `set the comment on table /local/app/records: YDB operation failed: ready=true status=BAD_REQUEST`},
		{name: "a read after a write that succeeded", alter: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS},
			confirm: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_UNAVAILABLE, Issues: budget},
			message: `confirm the comment on table /local/app/records: YDB operation failed: ready=true status=UNAVAILABLE`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize("COMMENT ON TABLE `app/records` IS 'rows'")
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			session := &attributeSession{alter: test.alter, confirm: test.confirm, described: &native.DescribeTableResult{}}
			comments := &nativeapi.Comments{Scheme: &describedPaths{entries: map[string]*Ydb_Scheme.Entry{"/local/app/records": {Name: "records", Type: Ydb_Scheme.Entry_TABLE}}},
				Open: func(context.Context) (nativeapi.AttributeSession, error) { return session, nil }}

			dispatched, err := comments.RunStatement(c.Context(), "/local", "/local", query, func(context.Context) error { return nil })

			c.Assert(dispatched, qt.IsTrue)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(ydbfailure.Read(err), qt.DeepEquals, test.want)
		})
	}
}
