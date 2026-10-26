package inspect_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func TestAccessReadsStreamingQueryIdentity(t *testing.T) {
	c := qt.New(t)
	snapshot := accessSnapshot()
	snapshot.NativeObjects = []*protocol.Envelope{{Kind: schema.StreamingQueryProperties,
		Owner: &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: "/local", Schema: "app", Name: "copy"}}}
	err := inspect.Access(c.Context(), snapshot, "/local", false, func(_ context.Context, path string) (*Ydb_Scheme.Entry, error) {
		if path == "/local" {
			return accessRoot("root"), nil
		}
		c.Assert(path, qt.Equals, "/local/app/copy")
		return &Ydb_Scheme.Entry{Name: "copy", Type: Ydb_Scheme.Entry_Type(26), Owner: "app"}, nil
	}, func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.CommonObjects.ObjectOwners, qt.HasLen, 2)
	ref := snapshot.CommonObjects.ObjectOwners[1].Object
	c.Assert(ref.Kind, qt.Equals, "streaming query")
	c.Assert(ref.Schema, qt.Equals, "app")
	c.Assert(ref.Name, qt.Equals, "copy")
}

func TestAccessPreservesIndependentMembershipsAndOwnerKnowledge(t *testing.T) {
	c := qt.New(t)
	snapshot := accessSnapshot()
	held := nativeapi.Principals{
		Users:       []nativeapi.User{{Name: "root", Enabled: true}, {Name: "app", Enabled: false}},
		Groups:      []string{"team", "DATA-READERS"},
		Memberships: []nativeapi.Membership{{Group: "DATA-READERS", Member: "app"}, {Group: "team", Member: "root"}},
	}
	var paths []string
	err := inspect.Access(c.Context(), snapshot, "/local", false, func(_ context.Context, path string) (*Ydb_Scheme.Entry, error) {
		paths = append(paths, path)
		return accessRoot("root"), nil
	}, func(context.Context) (nativeapi.Principals, error) { return held, nil })
	c.Assert(err, qt.IsNil)
	c.Assert(paths, qt.DeepEquals, []string{"/local"})
	objects := snapshot.CommonObjects
	c.Assert(objects.RolesOutOfScope, qt.DeepEquals, []string{"DATA-READERS", "root"})
	c.Assert(objects.Roles, qt.HasLen, 2)
	c.Assert(objects.Roles[0].Name, qt.Equals, "app")
	c.Assert(objects.Roles[0].Login, qt.IsFalse)
	c.Assert(objects.Roles[0].Password, qt.Equals, protocol.ObservedRole_PASSWORD_UNKNOWN)
	c.Assert(objects.Roles[1].Group, qt.IsTrue)
	c.Assert(objects.Memberships, qt.HasLen, 2)
	c.Assert(objects.Memberships[0].Group, qt.Equals, "DATA-READERS")
	c.Assert(objects.Memberships[1].Member, qt.Equals, "root")
	c.Assert(objects.ObjectOwners, qt.HasLen, 1)
	c.Assert(objects.ObjectOwners[0].Object.Kind, qt.Equals, "database")
	c.Assert(objects.ObjectOwners[0].OwnerCanLogin, qt.IsNotNil)
	c.Assert(*objects.ObjectOwners[0].OwnerCanLogin, qt.IsTrue)
	c.Assert(objects.Grants, qt.HasLen, 2)
	c.Assert(objects.Grants[0].Target.Name, qt.Equals, "")
	for _, coverage := range objects.Coverage {
		c.Assert(coverage.Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
	}
}

func TestAccessRefusalDoesNotBecomeAnEmptyPrincipalSet(t *testing.T) {
	c := qt.New(t)
	snapshot := accessSnapshot()
	err := inspect.Access(c.Context(), snapshot, "/local", false, func(context.Context, string) (*Ydb_Scheme.Entry, error) { return accessRoot("root"), nil },
		func(context.Context) (nativeapi.Principals, error) {
			return nativeapi.Principals{}, nativeapi.ErrPrincipalsRefused
		})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.CommonObjects.Grants, qt.HasLen, 2)
	c.Assert(snapshot.CommonObjects.ObjectOwners[0].OwnerCanLogin, qt.IsNil)
	c.Assert(snapshot.CommonObjects.Roles, qt.HasLen, 0)
	c.Assert(snapshot.CommonObjects.Memberships, qt.HasLen, 0)
	states := map[protocol.CommonObjectSupport_Kind]protocol.ModelKnowledge_State{}
	for _, record := range snapshot.CommonObjects.Coverage {
		states[record.Kind] = record.Knowledge.State
	}
	c.Assert(states, qt.DeepEquals, map[protocol.CommonObjectSupport_Kind]protocol.ModelKnowledge_State{
		protocol.CommonObjectSupport_GRANT:           protocol.ModelKnowledge_COMPLETE,
		protocol.CommonObjectSupport_OBJECT_OWNER:    protocol.ModelKnowledge_COMPLETE,
		protocol.CommonObjectSupport_ROLE:            protocol.ModelKnowledge_UNINSPECTED,
		protocol.CommonObjectSupport_ROLE_MEMBERSHIP: protocol.ModelKnowledge_UNINSPECTED,
	})
}

func TestAccessDoesNotInventLoginKnowledge(t *testing.T) {
	c := qt.New(t)
	snapshot := accessSnapshot()
	err := inspect.Access(c.Context(), snapshot, "/local", false, func(context.Context, string) (*Ydb_Scheme.Entry, error) { return accessRoot("external@directory"), nil },
		func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.CommonObjects.ObjectOwners[0].Owner, qt.Equals, "external@directory")
	c.Assert(snapshot.CommonObjects.ObjectOwners[0].OwnerCanLogin, qt.IsNil)
}

func TestAccessRealmKeepsDatabasePrincipalsOutOfScope(t *testing.T) {
	c := qt.New(t)
	snapshot := accessSnapshot()
	err := inspect.Access(c.Context(), snapshot, "/local/realm", true, func(context.Context, string) (*Ydb_Scheme.Entry, error) {
		entry := accessRoot("root")
		entry.Name, entry.Type = "realm", Ydb_Scheme.Entry_DIRECTORY
		return entry, nil
	}, func(context.Context) (nativeapi.Principals, error) {
		return nativeapi.Principals{Users: []nativeapi.User{{Name: "app", Enabled: true}}, Groups: []string{"team"},
			Memberships: []nativeapi.Membership{{Group: "team", Member: "app"}}}, nil
	})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.CommonObjects.Roles, qt.HasLen, 0)
	c.Assert(snapshot.CommonObjects.RolesOutOfScope, qt.DeepEquals, []string{"app", "team"})
	c.Assert(snapshot.CommonObjects.Memberships, qt.HasLen, 1)
}

func TestAccessFailureReturnsNoPartialProjection(t *testing.T) {
	for _, test := range []struct {
		name                      string
		entry                     *Ydb_Scheme.Entry
		readError, principalError error
	}{
		{name: "access read", readError: errors.New("read failed")},
		{name: "wrong path", entry: &Ydb_Scheme.Entry{Name: "other", Type: Ydb_Scheme.Entry_DATABASE}},
		{name: "wrong kind", entry: &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_TABLE}},
		{name: "principal read", entry: accessRoot("root"), principalError: errors.New("principal stream failed")},
		{name: "invalid permission", entry: &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE, Permissions: []*Ydb_Scheme.Permissions{{PermissionNames: []string{"ydb.generic.read"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			snapshot := accessSnapshot()
			before := proto.CloneOf(snapshot)
			err := inspect.Access(c.Context(), snapshot, "/local", false,
				func(context.Context, string) (*Ydb_Scheme.Entry, error) { return test.entry, test.readError },
				func(context.Context) (nativeapi.Principals, error) {
					return nativeapi.Principals{}, test.principalError
				})
			c.Assert(err, qt.IsNotNil)
			c.Assert(proto.Equal(snapshot, before), qt.IsTrue)
		})
	}
}

func accessSnapshot() *protocol.Schema {
	return &protocol.Schema{Complete: true, Engine: "ydb", CommonObjects: &protocol.ObservedCommonObjects{}}
}
func accessRoot(owner string) *Ydb_Scheme.Entry {
	return &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE, Owner: owner,
		Permissions: []*Ydb_Scheme.Permissions{{Subject: "DATA-READERS", PermissionNames: []string{"ydb.generic.read", "ydb.generic.list", "ydb.generic.read"}}}}
}
