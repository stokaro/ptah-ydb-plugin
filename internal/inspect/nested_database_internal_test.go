package inspect

// White-box testing required: the directory walk runs only inside
// ReadObjects, which opens a table session on a YDB driver before it lists
// anything, so no offline caller reaches it. The walk lists through the scheme
// client alone, which a fake answers here. The live test
// TestRootReadLeavesANestedDatabaseOutSubprocess drives the public read against
// a server that serves a nested database.
import (
	"context"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// schemeTree answers the listing of each directory it holds and records every
// path it is asked to list. A call other than ListDirectory reaches the
// embedded nil interface and panics.
type schemeTree struct {
	Ydb_Scheme_V1.SchemeServiceClient
	answers map[string]*Ydb_Operations.Operation
	listed  []string
}

func (s *schemeTree) ListDirectory(_ context.Context, request *Ydb_Scheme.ListDirectoryRequest, _ ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	s.listed = append(s.listed, request.Path)
	return &Ydb_Scheme.ListDirectoryResponse{Operation: s.answers[request.Path]}, nil
}

// nestedDatabaseTree is the database /local as YDB lists it while it holds the
// database db2 at its root and the database inner in its directory app, beside
// the server's .sys.
func nestedDatabaseTree(c *qt.C) *schemeTree {
	c.Helper()
	tree := &schemeTree{answers: make(map[string]*Ydb_Operations.Operation)}
	for absolute, listing := range map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local": {Self: &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE}, Children: []*Ydb_Scheme.Entry{
			{Name: "db2", Type: Ydb_Scheme.Entry_DATABASE}, {Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}, {Name: ".sys", Type: Ydb_Scheme.Entry_DIRECTORY},
		}},
		"/local/app": {Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}, Children: []*Ydb_Scheme.Entry{
			{Name: "inner", Type: Ydb_Scheme.Entry_DATABASE},
		}},
	} {
		payload, err := anypb.New(listing)
		c.Assert(err, qt.IsNil)
		tree.answers[absolute] = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
	}
	return tree
}

// A database nested below the one read is another database's root, not a
// directory of this one: the walk neither descends into it nor reads it as an
// object, at the root and in a directory alike, as the host's embedded engine
// left it out. Read as an object, it refused the whole database (`YDB object
// kind DATABASE needs an extracted reader`).
func TestWalkLeavesANestedDatabaseOut(t *testing.T) {
	for _, test := range []struct {
		name            string
		selected        string
		wantDirectories []string
	}{
		{name: "at the root", selected: "", wantDirectories: []string{"app"}},
		{name: "in a directory", selected: "app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			tree := nestedDatabaseTree(c)
			read := &traversal{source: &source{scheme: tree}, database: "/local",
				result: &protocol.Schema{Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}}

			directories, err := read.directory(c.Context(), test.selected)

			c.Assert(err, qt.IsNil)
			c.Assert(directories, qt.DeepEquals, test.wantDirectories)
			c.Assert(tree.listed, qt.DeepEquals, []string{path.Join("/local", test.selected)})
			c.Assert(read.result.Tables, qt.HasLen, 0)
			c.Assert(read.result.NativeObjects, qt.HasLen, 0)
			c.Assert(read.result.ReservedObjects, qt.HasLen, 0)
			c.Assert(read.skippedRealm, qt.IsFalse)
		})
	}
}
