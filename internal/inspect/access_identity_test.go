package inspect_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// noPrincipals answers a read of the users and groups with none.
func noPrincipals(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil }

// TestAccessReadsReplicationIdentity_HappyPath pins that the owner and the
// grants of an async replication and of a transfer are read back to the
// object's own identity: the grant object kind a grant on it names, its
// directory and its name. Ported from the host's TestSchemeObjects_HappyPath.
func TestAccessReadsReplicationIdentity_HappyPath(t *testing.T) {
	tests := []struct {
		kind, accessKind string
		entry            Ydb_Scheme.Entry_Type
	}{
		{kind: schema.ReplicationProperties, accessKind: "async replication", entry: Ydb_Scheme.Entry_REPLICATION},
		{kind: schema.TransferProperties, accessKind: "transfer", entry: Ydb_Scheme.Entry_TRANSFER},
	}
	for _, test := range tests {
		t.Run(test.accessKind, func(t *testing.T) {
			c := testcheck.New(t)
			snapshot := accessSnapshot()
			snapshot.NativeObjects = []*protocol.Envelope{{Kind: test.kind,
				Owner: &protocol.Reference{Engine: "ydb", Kind: test.kind, Database: "/local", Schema: "app/eu", Name: "events"}}}
			err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, described(map[string]*Ydb_Scheme.Entry{
				"/local": accessRoot("root"),
				"/local/app/eu/events": {Name: "events", Type: test.entry, Owner: "app",
					Permissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.granular.select_row"}}}},
			}), noPrincipals)
			c.Assert(err, qt.IsNil)
			objects := snapshot.CommonObjects
			c.Assert(objects.ObjectOwners, qt.HasLen, 2)
			owned := objects.ObjectOwners[1]
			c.Assert([]string{owned.Owner, owned.Object.Kind, owned.Object.Database, owned.Object.Schema, owned.Object.Name}, qt.DeepEquals,
				[]string{"app", test.accessKind, "/local", "app/eu", "events"})
			c.Assert(objects.Grants, qt.HasLen, 3)
			granted := objects.Grants[2]
			c.Assert([]string{granted.Role, granted.Privilege, granted.Target.Kind, granted.Target.Database, granted.Target.Schema, granted.Target.Name}, qt.DeepEquals,
				[]string{"reader", "ydb.granular.select_row", test.accessKind, "/local", "app/eu", "events"})
		})
	}
}

// TestAccessGivesAClassifierNoAccessList_HappyPath pins that a resource pool
// classifier, which has no scheme object of its own and no grant names, is
// not described and holds no owner or grant: the read records the database's
// alone. Ported from the host's TestSchemeObjects_FailurePath.
func TestAccessGivesAClassifierNoAccessList_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	snapshot := accessSnapshot()
	snapshot.NativeObjects = []*protocol.Envelope{{Kind: schema.ResourcePoolClassifierProperties,
		Owner: &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Database: "/local", Name: "etl"}}}
	var paths []string
	err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, func(_ context.Context, path string) (*Ydb_Scheme.Entry, error) {
		paths = append(paths, path)
		return accessRoot("root"), nil
	}, noPrincipals)
	c.Assert(err, qt.IsNil)
	c.Assert(paths, qt.DeepEquals, []string{"/local"})
	c.Assert(snapshot.CommonObjects.ObjectOwners, qt.HasLen, 1)
	c.Assert(snapshot.CommonObjects.ObjectOwners[0].Object.Kind, qt.Equals, "database")
	c.Assert(snapshot.CommonObjects.Grants, qt.HasLen, 2)
}
