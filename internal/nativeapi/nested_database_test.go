package nativeapi_test

// These tests list paths at and below a database nested in the session's
// database, as YDB answers a session of /local while /local/db2 is served
// beside it: the parent lists db2 as a DATABASE, db2 lists itself as one, and
// a path below db2 answers SCHEME_ERROR.

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// nestedDatabase is the database /local holding the database db2 at its root
// and the directory app, which holds no database.
func nestedDatabase() *schemeTree {
	return &schemeTree{directories: map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local": listing("/local", Ydb_Scheme.Entry_DATABASE, &Ydb_Scheme.Entry{Name: "db2", Type: Ydb_Scheme.Entry_DATABASE},
			&Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}),
		"/local/db2": listing("/local/db2", Ydb_Scheme.Entry_DATABASE, &Ydb_Scheme.Entry{Name: "inner", Type: Ydb_Scheme.Entry_DIRECTORY}),
		"/local/app": listing("/local/app", Ydb_Scheme.Entry_DIRECTORY),
	}}
}

// A path at or below a nested database is refused with the nested database's
// root, relative to the session's database. Without the refusal the root
// reads as a directory, and a path below it as one whose parent is not a
// directory.
func TestListDirectoryInANestedDatabase_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, relative, want, wantPath string }{
		{name: "the nested database's root", relative: "db2",
			want: `list YDB directory /local/db2: db2 is another YDB database, nested in this one`, wantPath: "/local/db2"},
		{name: "a directory the nested database holds", relative: "db2/inner",
			want: `list YDB directory /local/db2/inner: db2 is another YDB database, nested in this one`, wantPath: "/local/db2/inner"},
		{name: "a directory two levels below the root", relative: "db2/inner/deeper",
			want: `list YDB directory /local/db2/inner/deeper: db2 is another YDB database, nested in this one`, wantPath: "/local/db2/inner/deeper"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			listed, err := nativeapi.ListDirectory(c.Context(), nativeapi.SchemeLister(nestedDatabase()), "/local", test.relative)
			c.Assert(err, qt.ErrorMatches, test.want)
			nested, isNested := errors.AsType[*nativeapi.NestedDatabaseError](err)
			c.Assert(isNested, qt.IsTrue)
			c.Assert(nested.Root, qt.Equals, "db2")
			c.Assert(nested.Path, qt.Equals, test.wantPath)
			c.Assert(err, qt.Not(qt.ErrorIs), nativeapi.ErrDirectoryNotVisible)
			c.Assert(listed, qt.IsNil)
		})
	}
}

// The database's own root and a directory beside the nested database read as
// they do on a server that serves none.
func TestListDirectoryBesideANestedDatabase_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, relative, wantName string
		wantType                 Ydb_Scheme.Entry_Type
	}{
		{name: "the database root", wantName: "local", wantType: Ydb_Scheme.Entry_DATABASE},
		{name: "a directory beside it", relative: "app", wantName: "app", wantType: Ydb_Scheme.Entry_DIRECTORY},
		{name: "a new directory beside it", relative: "app/fresh", wantName: "fresh", wantType: Ydb_Scheme.Entry_DIRECTORY},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			listed, err := nativeapi.ListDirectory(c.Context(), nativeapi.SchemeLister(nestedDatabase()), "/local", test.relative)
			c.Assert(err, qt.IsNil)
			c.Assert(listed.GetSelf().GetName(), qt.Equals, test.wantName)
			c.Assert(listed.GetSelf().GetType(), qt.Equals, test.wantType)
		})
	}
}

// A topic, a coordination node and a row table declared in the nested
// database are refused before anything is sent. Without the refusal the
// topic's and the node's reads take the nested database's root for a
// directory, the creation runs through the outer session, and YDB writes the
// object into the nested database.
func TestTopicCreationInANestedDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	topic := &nativeapi.Topic{Scheme: nestedDatabase(), Execute: sent.execute}
	dispatched, err := topic.Apply(c.Context(), "/local", topicCreation(c, "db2"), allow)
	c.Assert(err, qt.ErrorMatches, "read topic db2/events: list YDB directory /local/db2: db2 is another YDB database, nested in this one")
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(sent.sent, qt.HasLen, 0)
}

func TestCoordinationCreationInANestedDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	writes := &nodeWrites{}
	nodes := &nativeapi.Coordination{Scheme: nestedDatabase(), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(writes)}
	dispatched, err := nodes.Apply(c.Context(), "/local", coordinationCreation(c, "db2/inner"), allow)
	c.Assert(err, qt.ErrorMatches, ".*list YDB directory /local/db2/inner: db2 is another YDB database, nested in this one")
	c.Assert(err, qt.ErrorAs, new(*nativeapi.NestedDatabaseError))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(writes.methods, qt.HasLen, 0)
}

func TestRowTableCreationInANestedDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	session := &tableWrites{}
	creation := &nativeapi.TableCreation{Scheme: nestedDatabase(), Open: func(context.Context) (nativeapi.CreationSession, error) { return session, nil }}
	dispatched, err := creation.Apply(c.Context(), "/local", rowTableCreation(c, "db2"), allow)
	c.Assert(err, qt.ErrorMatches, "read table db2/records: list YDB directory /local/db2: db2 is another YDB database, nested in this one")
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(session.created, qt.HasLen, 0)
}
