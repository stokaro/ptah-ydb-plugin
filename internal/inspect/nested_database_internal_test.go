package inspect

// White-box testing required: the directory walk runs only inside
// ReadObjects, which opens a table session on a YDB driver before it lists
// anything, so no offline caller reaches it. The walk lists through the scheme
// client alone, which a fake answers here. The live test
// TestRootReadLeavesANestedDatabaseOutSubprocess drives the public read against
// a server that serves a nested database.
import (
	"context"
	"fmt"
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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

// nestedDatabaseTree is the database /local as YDB lists it to a session of
// /local while it holds the database db2 at its root and the database inner in
// its directory app, beside the server's .sys: each nested database lists
// itself as a DATABASE, and a path below one answers SCHEME_ERROR.
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
		"/local/db2":       {Self: &Ydb_Scheme.Entry{Name: "db2", Type: Ydb_Scheme.Entry_DATABASE}},
		"/local/app/inner": {Self: &Ydb_Scheme.Entry{Name: "inner", Type: Ydb_Scheme.Entry_DATABASE}},
	} {
		payload, err := anypb.New(listing)
		c.Assert(err, qt.IsNil)
		tree.answers[absolute] = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
	}
	for _, absolute := range []string{"/local/db2/inner", "/local/db2/inner/deeper", "/local/app/inner/deep"} {
		tree.answers[absolute] = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SCHEME_ERROR}
	}
	return tree
}

// nestedTraversal is a read of /local over tree, with the coverage of every
// model a read reports.
func nestedTraversal(tree *schemeTree) *traversal {
	result := &protocol.Schema{Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
	for _, model := range schema.ObservedModels() {
		result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ModelKindCoverage{Model: model,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Knowledge is limited to returned subjects and fully listed directories"}})
	}
	return &traversal{source: &source{scheme: tree}, database: "/local", result: result}
}

// claims counts the kinds of result that make each claim, written as the
// claimed directory, whether the claim covers the directories below it, and
// its knowledge.
func claims(result *protocol.Schema) map[string]int {
	counted := make(map[string]int)
	for _, record := range result.GetCoverage().GetKinds() {
		for _, region := range record.GetRegions() {
			counted[fmt.Sprintf("%s subtree=%t %s: %s", region.GetScope().GetSchema().GetNormalized(), region.GetScope().GetSchemaSubtree(),
				region.GetKnowledge().GetState(), region.GetKnowledge().GetReason())]++
		}
	}
	return counted
}

// kindsCounted is the number of models a read reports whose kind counted
// selects.
func kindsCounted(counted func(string) bool) int {
	return len(slices.DeleteFunc(slices.Clone(schema.ObservedModels()), func(model *protocol.ValueModel) bool { return !counted(model.Kind) }))
}

// nativeOrFacet selects a native kind or a table facet.
func nativeOrFacet(kind string) bool {
	return schema.IsNativeObject(kind) || slices.Contains(tablecompare.Kinds(), kind)
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

// A selector that names a nested database, or a directory below one, reads as
// an empty directory, as the host's embedded engine read it: its walk never
// entered a nested database. The directory is claimed complete for every
// native kind and table facet, with the nested database's reason, which the
// comparison and the planning check read back to refuse a creation there.
// Read as a directory to list, the selection is refused (`YDB schema selector
// did not describe the expected directory` at the root, `its parent is not a
// directory` below it).
func TestSelectedNestedDatabaseReadsAsAnEmptyDirectory(t *testing.T) {
	for _, test := range []struct{ name, selected, root string }{
		{name: "the root of a nested database", selected: "db2", root: "db2"},
		{name: "a directory below it", selected: "db2/inner", root: "db2"},
		{name: "two directories below it", selected: "db2/inner/deeper", root: "db2"},
		{name: "a nested database in a directory", selected: "app/inner", root: "app/inner"},
		{name: "a directory below that one", selected: "app/inner/deep", root: "app/inner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := nestedTraversal(nestedDatabaseTree(c))

			directories, err := read.directory(c.Context(), test.selected)

			c.Assert(err, qt.IsNil)
			c.Assert(directories, qt.HasLen, 0)
			c.Assert(read.result.Tables, qt.HasLen, 0)
			c.Assert(read.result.NativeObjects, qt.HasLen, 0)
			c.Assert(read.nested, qt.DeepEquals, []string{test.root})
			c.Assert(claims(read.result), qt.DeepEquals, map[string]int{
				test.selected + " subtree=false COMPLETE: Directory " + test.root + " is the root of another YDB database, nested in this one": kindsCounted(nativeOrFacet),
			})
		})
	}
}

// A read of the whole database claims the root of each nested database it
// left out for the native kinds, with the nested database's reason, and with
// subtree the directories below it too where the host takes subtree regions.
// The table facets get no claim there, as no directory the walk did not list
// gets one.
func TestWholeReadClaimsEachNestedDatabase(t *testing.T) {
	for _, test := range []struct {
		name    string
		subtree bool
	}{
		{name: "with subtree regions", subtree: true},
		{name: "with exact regions", subtree: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := nestedTraversal(nestedDatabaseTree(c))
			directories, err := read.directory(c.Context(), "")
			c.Assert(err, qt.IsNil)
			c.Assert(directories, qt.DeepEquals, []string{"app"})
			_, err = read.directory(c.Context(), "app")
			c.Assert(err, qt.IsNil)
			before := claims(read.result)

			read.nestedRegions(test.subtree)

			claimed := claims(read.result)
			for description := range before {
				delete(claimed, description)
			}
			natives := kindsCounted(schema.IsNativeObject)
			c.Assert(read.nested, qt.DeepEquals, []string{"db2", "app/inner"})
			c.Assert(claimed, qt.DeepEquals, map[string]int{
				fmt.Sprintf("db2 subtree=%t COMPLETE: Directory db2 is the root of another YDB database, nested in this one", test.subtree):             natives,
				fmt.Sprintf("app/inner subtree=%t COMPLETE: Directory app/inner is the root of another YDB database, nested in this one", test.subtree): natives,
			})
		})
	}
}
