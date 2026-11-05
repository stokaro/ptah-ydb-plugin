package nativeapi_test

import (
	"context"
	"errors"
	"fmt"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// listing is the directory listing YDB answers for absolute.
func listing(absolute string, kind Ydb_Scheme.Entry_Type, children ...*Ydb_Scheme.Entry) *Ydb_Scheme.ListDirectoryResult {
	return &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: path.Base(absolute), Type: kind}, Children: children}
}

// schemeTree is a scheme service over the directories it holds: it answers
// every other path with SCHEME_ERROR, as YDB answers a path it does not show,
// and records each path it is asked to list. A call other than ListDirectory
// reaches the embedded nil interface and panics.
type schemeTree struct {
	Ydb_Scheme_V1.SchemeServiceClient
	directories map[string]*Ydb_Scheme.ListDirectoryResult
	listed      []string
}

func (s *schemeTree) ListDirectory(_ context.Context, request *Ydb_Scheme.ListDirectoryRequest, _ ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	s.listed = append(s.listed, request.Path)
	held, found := s.directories[request.Path]
	if !found {
		return &Ydb_Scheme.ListDirectoryResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SCHEME_ERROR}}, nil
	}
	return &Ydb_Scheme.ListDirectoryResponse{Operation: result(held)}, nil
}

// newDirectory is a database that holds an app directory with a topic, a
// hidden directory the session may not list, and a table, and no fresh
// directory.
func newDirectory() *schemeTree {
	return &schemeTree{directories: map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local": listing("/local", Ydb_Scheme.Entry_DATABASE, &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY},
			&Ydb_Scheme.Entry{Name: "hidden", Type: Ydb_Scheme.Entry_DIRECTORY}, &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE}),
		"/local/app":     listing("/local/app", Ydb_Scheme.Entry_DIRECTORY, &Ydb_Scheme.Entry{Name: "events", Type: Ydb_Scheme.Entry_TOPIC}),
		"/local/records": listing("/local/records", Ydb_Scheme.Entry_TABLE),
	}}
}

// failingRoot answers the listing of /local with failure and every other path
// as YDB answers one it does not show.
func failingRoot(failure error) nativeapi.DirectoryLister {
	return func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		if absolute == "/local" {
			return nil, failure
		}
		return nil, fmt.Errorf("list YDB directory %s: %w", absolute, nativeapi.ErrDirectoryNotVisible)
	}
}

func TestListDirectory_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, database, relative, wantName string
		wantType                           Ydb_Scheme.Entry_Type
		wantChildren                       int
		wantListed                         []string
	}{
		{name: "the database root", database: "/local", wantName: "local", wantType: Ydb_Scheme.Entry_DATABASE, wantChildren: 3,
			wantListed: []string{"/local"}},
		{name: "an existing directory", database: "/local", relative: "app", wantName: "app", wantType: Ydb_Scheme.Entry_DIRECTORY, wantChildren: 1,
			wantListed: []string{"/local/app"}},
		{name: "a directory its parent does not name", database: "/local", relative: "fresh", wantName: "fresh", wantType: Ydb_Scheme.Entry_DIRECTORY,
			wantListed: []string{"/local/fresh", "/local"}},
		{name: "a directory below one that does not exist", database: "/local", relative: "fresh/nested", wantName: "nested", wantType: Ydb_Scheme.Entry_DIRECTORY,
			wantListed: []string{"/local/fresh/nested", "/local/fresh", "/local"}},
		{name: "a directory below an existing one", database: "/local", relative: "app/fresh", wantName: "fresh", wantType: Ydb_Scheme.Entry_DIRECTORY,
			wantListed: []string{"/local/app/fresh", "/local/app"}},
		{name: "a directory of a realm root", database: "/local/app", relative: "fresh", wantName: "fresh", wantType: Ydb_Scheme.Entry_DIRECTORY,
			wantListed: []string{"/local/app/fresh", "/local/app"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			scheme := newDirectory()
			listed, err := nativeapi.ListDirectory(c.Context(), nativeapi.SchemeLister(scheme), test.database, test.relative)
			c.Assert(err, qt.IsNil)
			c.Assert(listed.GetSelf().GetName(), qt.Equals, test.wantName)
			c.Assert(listed.GetSelf().GetType(), qt.Equals, test.wantType)
			c.Assert(listed.GetChildren(), qt.HasLen, test.wantChildren)
			c.Assert(scheme.listed, qt.DeepEquals, test.wantListed)
		})
	}
}

func TestListDirectory_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, database, relative, want string }{
		{name: "a directory its parent names", database: "/local", relative: "hidden",
			want: `list YDB directory /local/hidden: its parent lists it, and YDB does not show it to the session: YDB directory does not exist or is not visible`},
		{name: "a directory below one its parent names", database: "/local", relative: "hidden/nested",
			want: `list YDB directory /local/hidden: its parent lists it, and YDB does not show it to the session: YDB directory does not exist or is not visible`},
		{name: "the database root", database: "/missing", want: `list YDB directory /missing: YDB directory does not exist or is not visible`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			listed, err := nativeapi.ListDirectory(c.Context(), nativeapi.SchemeLister(newDirectory()), test.database, test.relative)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
			c.Assert(listed, qt.IsNil)
		})
	}
	t.Run("a directory below a table", func(t *testing.T) {
		c := testcheck.New(t)
		listed, err := nativeapi.ListDirectory(c.Context(), nativeapi.SchemeLister(newDirectory()), "/local", "records/nested")
		c.Assert(err, qt.ErrorMatches, `list YDB directory /local/records/nested: its parent is not a directory`)
		c.Assert(err, qt.Not(qt.ErrorIs), nativeapi.ErrDirectoryNotVisible)
		c.Assert(listed, qt.IsNil)
	})
	t.Run("a failed parent listing", func(t *testing.T) {
		c := testcheck.New(t)
		failure := errors.New("list YDB directory /local: transport failed")
		listed, err := nativeapi.ListDirectory(c.Context(), failingRoot(failure), "/local", "fresh")
		c.Assert(err, qt.ErrorIs, failure)
		c.Assert(listed, qt.IsNil)
	})
}
