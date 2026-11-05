package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
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
