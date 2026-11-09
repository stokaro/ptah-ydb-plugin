package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// refusedAccessCapabilities are the access facts every line the provider
// serves states, with relative grant paths and every native object kind a
// grant names.
func refusedAccessCapabilities() map[string]bool {
	capabilities := grantLineCapabilities(true)
	capabilities["group_principals"], capabilities["role_membership"] = true, true
	return capabilities
}

// TestRenderAccessChangeRefusal_FailurePath pins the access changes YDB has
// no statement for, and the ones a target without the capability key they
// need cannot make: a group's login, a change of a principal's kind, a grant
// option, which YDB keeps as the permission ydb.access.grant, and a
// membership, a group or a permission on the database without its key. Every
// line the provider serves states those keys; the rows pin what a target
// that states one false meets. Ported from the host's
// TestGenerateMigrationAST_AccessChanges_FailurePath.
func TestRenderAccessChangeRefusal_FailurePath(t *testing.T) {
	onTable := &protocol.Reference{Engine: "ydb", Kind: "table", Name: "t"}
	optioned := &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: onTable, WithGrantOption: true}
	onDatabase := &protocol.SQLGrant{Role: "app", Privileges: []string{"CONNECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: "database", Database: "/local"}}
	membership := &protocol.RoleMembership{Group: "readers", Member: "app"}
	tests := []struct {
		name    string
		change  *protocol.CommonObjectChange
		without string
		want    string
	}{
		{name: "a group's login", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{
			Desired: &protocol.SQLRole{Name: "readers", Group: true, Login: true}, Changed: []string{"login"}}}},
			want: `principal "readers" declares login, which a YDB user or group does not carry; a group never logs in`},
		{name: "a change of a principal's kind", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{
			Desired: &protocol.SQLRole{Name: "app"}, Changed: []string{"group"}}}},
			want: "ALTER USER changes group, which a YDB user does not carry"},
		{name: "a grant option granted", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: optioned}},
			want: "YDB stores the grant option as permission ydb.access.grant; grant or revoke that permission directly"},
		{name: "a grant option revoked", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: optioned}},
			want: "YDB stores the grant option as permission ydb.access.grant; grant or revoke that permission directly"},
		{name: "a membership added without the key", without: "role_membership",
			change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AddMember{AddMember: membership}},
			want:   "YDB access operation requires target capability role_membership"},
		{name: "a membership removed without the key", without: "role_membership",
			change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_RemoveMember{RemoveMember: membership}},
			want:   "YDB access operation requires target capability role_membership"},
		{name: "a group created without the key", without: "group_principals",
			change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "readers", Group: true}}}},
			want:   "YDB access operation requires target capability group_principals"},
		{name: "a group dropped without the key", without: "group_principals",
			change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropRole{DropRole: &protocol.Reference{Kind: "group", Name: "readers"}}},
			want:   "YDB access operation requires target capability group_principals"},
		{name: "a permission on the database without the key", without: "database_grants",
			change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: onDatabase}},
			want:   "YDB access operation requires target capability database_grants"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			capabilities := refusedAccessCapabilities()
			delete(capabilities, test.without)
			got, err := commonobject.Render(test.change, capabilities)
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, test.want)
			c.Assert(got, qt.Equals, "")
		})
	}
}

// TestRenderGrantNeedsItsObjectsCapability_FailurePath pins the capability
// key each native object kind's access list exists under: a GRANT or a REVOKE
// on an object of the kind is refused, naming the key, on a target that does
// not state it. An external table's key is the external data source's.
// Ported from the host's TestGenerateMigrationAST_ObjectPermissions_FailurePath
// and the capability column of TestSchemeObjects_HappyPath.
func TestRenderGrantNeedsItsObjectsCapability_FailurePath(t *testing.T) {
	tests := []struct {
		kind, directory, capability string
	}{
		{kind: "topic", directory: "app", capability: "topics"},
		{kind: "coordination node", directory: "app", capability: "coordination_nodes"},
		{kind: "secret", directory: "ext", capability: "secrets"},
		{kind: "external data source", directory: "ext", capability: "external_data_sources"},
		{kind: "external table", directory: "ext", capability: "external_data_sources"},
		{kind: "async replication", directory: "app", capability: "async_replication"},
		{kind: "transfer", directory: "app", capability: "transfers"},
		{kind: "streaming query", directory: "app", capability: "streaming_queries"},
		{kind: "resource pool", capability: "resource_pools"},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			c := testcheck.New(t)
			target := &protocol.Reference{Engine: "ydb", Kind: test.kind, Schema: test.directory, Name: "events"}
			declared := &protocol.SQLGrant{Role: "reader", Privileges: []string{"SELECT ROW"}, Target: target}
			capabilities := refusedAccessCapabilities()
			capabilities[test.capability] = false
			for _, change := range []*protocol.CommonObjectChange{
				{Operation: &protocol.CommonObjectChange_Grant{Grant: declared}},
				{Operation: &protocol.CommonObjectChange_Revoke{Revoke: declared}},
			} {
				got, err := commonobject.Render(change, capabilities)
				c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
				c.Assert(err.Error(), qt.Contains, "YDB access operation requires target capability "+test.capability)
				c.Assert(got, qt.Equals, "")
			}
		})
	}
}
