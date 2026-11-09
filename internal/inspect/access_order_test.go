package inspect_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// orderedGrant is one grant a read reports: the object, the role and the
// permission.
type orderedGrant struct{ Target, Role, Privilege string }

// A read reports grants in the order the host's embedded engine at 74de0b65b
// read them: a resource pool's first, then the database's, then each
// directory before the entries in it, the entries of a directory by name, and
// the entries of one object as YDB describes them, which is the order they
// were granted. Comparing whole paths put the table a.b ahead of the table
// in the directory a, and sorting put a grant by its role and permission.
func TestAccessReportsGrantsInReadingOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	snapshot := accessDirectory("a")
	table := func(directory, name string) *protocol.Table {
		return &protocol.Table{Identity: &protocol.Reference{Engine: "ydb", Database: "/local", Kind: "table", Schema: directory, Name: name}}
	}
	snapshot.Tables = []*protocol.Table{table("", "a.b"), table("a", "x")}
	snapshot.NativeObjects = []*protocol.Envelope{{Kind: schema.ResourcePoolProperties,
		Owner: &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Database: "/local", Name: "batch"}}}
	grant := func(subject, permission string) *Ydb_Scheme.Permissions {
		return &Ydb_Scheme.Permissions{Subject: subject, PermissionNames: []string{permission}}
	}
	entries := map[string]*Ydb_Scheme.Entry{
		"/local":   accessRoot("root"),
		"/local/a": {Name: "a", Type: Ydb_Scheme.Entry_DIRECTORY, Permissions: []*Ydb_Scheme.Permissions{grant("lister", "ydb.generic.list")}},
		"/local/a/x": {Name: "x", Type: Ydb_Scheme.Entry_TABLE, Permissions: []*Ydb_Scheme.Permissions{
			grant("writer", "ydb.granular.update_row"), grant("writer", "ydb.granular.select_row"), grant("reader", "ydb.granular.select_row"),
		}},
		"/local/a.b": {Name: "a.b", Type: Ydb_Scheme.Entry_TABLE, Permissions: []*Ydb_Scheme.Permissions{grant("reader", "ydb.granular.select_row")}},
		"/local/.metadata/workload_manager/pools/batch": {Name: "batch", Type: Ydb_Scheme.Entry_RESOURCE_POOL,
			Permissions: []*Ydb_Scheme.Permissions{grant("pooled", "ydb.granular.describe_schema")}},
	}

	err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, described(entries),
		func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })

	c.Assert(err, qt.IsNil)
	var grants []orderedGrant
	for _, granted := range snapshot.CommonObjects.Grants {
		grants = append(grants, orderedGrant{granted.Target.Kind + " " + granted.Target.Schema + "/" + granted.Target.Name, granted.Role, granted.Privilege})
	}
	c.Assert(grants, qt.DeepEquals, []orderedGrant{
		{"resource pool /batch", "pooled", "ydb.granular.describe_schema"},
		{"database /", "DATA-READERS", "ydb.generic.read"},
		{"database /", "DATA-READERS", "ydb.generic.list"},
		{"schema /a", "lister", "ydb.generic.list"},
		{"table a/x", "writer", "ydb.granular.update_row"},
		{"table a/x", "writer", "ydb.granular.select_row"},
		{"table a/x", "reader", "ydb.granular.select_row"},
		{"table /a.b", "reader", "ydb.granular.select_row"},
	})
}

// A read reports the users and the groups together by name, and the
// memberships by group and then member, as the YDB engine built into the host
// at 74de0b65b sorted them, so a render writes a group ahead of a user whose
// name sorts after it. The read kept the users ahead of the groups, each in
// the order its system view lists them.
func TestAccessReportsPrincipalsByName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	snapshot := accessSnapshot()
	held := nativeapi.Principals{
		Users:       []nativeapi.User{{Name: "zed", Enabled: true}, {Name: "amy", Enabled: true}},
		Groups:      []string{"beta", "alpha"},
		Memberships: []nativeapi.Membership{{Group: "beta", Member: "zed"}, {Group: "alpha", Member: "zed"}, {Group: "alpha", Member: "amy"}},
	}

	err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, described(map[string]*Ydb_Scheme.Entry{"/local": accessRoot("root")}),
		func(context.Context) (nativeapi.Principals, error) { return held, nil })

	c.Assert(err, qt.IsNil)
	var roles, memberships []string
	for _, role := range snapshot.CommonObjects.Roles {
		roles = append(roles, role.Name)
	}
	for _, membership := range snapshot.CommonObjects.Memberships {
		memberships = append(memberships, membership.Group+" "+membership.Member)
	}
	c.Assert(roles, qt.DeepEquals, []string{"alpha", "amy", "beta", "zed"})
	c.Assert(memberships, qt.DeepEquals, []string{"alpha amy", "alpha zed", "beta zed"})
}
