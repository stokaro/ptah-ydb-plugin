package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// TestDeclaredGrantRefusal_FailurePath pins the declared grants and revokes
// YDB's access lists cannot hold, each with its reason: a sequence, a
// routine, columns, the grant option, no privilege, a privilege YDB does not
// have, an object kind YDB has not, which a resource pool classifier is
// among, an object with no name or an inexact path, and a resource pool named
// by a path rather than by its name alone. The routine is granted USE, a
// permission YDB has: EXECUTE is refused first as a permission YDB does not
// have. Ported from the host's
// TestValidateDeclared_FailurePath and TestSchemeObjects_FailurePath.
func TestDeclaredGrantRefusal_FailurePath(t *testing.T) {
	onTable := &protocol.Reference{Engine: "ydb", Kind: "table", Name: "t"}
	tests := []struct {
		name  string
		grant *protocol.SQLGrant
		want  string
	}{
		{name: "a sequence", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "sequence", Name: "s"}},
			want: `a YDB grant cannot name object kind "sequence"`},
		{name: "a routine", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "function", Name: "f", Overload: []string{"int"}}},
			want: "a YDB grant target has an engine, a directory and an object name, without a catalog, parent or routine signature"},
		{name: "columns", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: onTable, Columns: []string{"a", "b"}},
			want: "a YDB permission is on a whole object, not on columns"},
		{name: "the grant option", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: onTable, WithGrantOption: true},
			want: "YDB stores the grant option as permission ydb.access.grant; grant or revoke that permission directly"},
		{name: "no privilege", grant: &protocol.SQLGrant{Role: "app", Target: onTable},
			want: "a YDB grant names no permission"},
		{name: "a privilege YDB does not have, in a revoke", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"TRUNCATE"}, Target: onTable, Revoked: true},
			want: `"TRUNCATE" is not a YDB permission`},
		{name: "a scheme object of a kind YDB has not", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT ROW"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "QUEUE", Schema: "shop", Name: "q"}},
			want: `a YDB grant cannot name object kind "QUEUE"`},
		{name: "a resource pool classifier", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool classifier", Name: "etl"}},
			want: `a YDB grant cannot name object kind "resource pool classifier"`},
		{name: "a topic with no name", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT ROW"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "topic"}},
			want: "a YDB grant requires an exact object path"},
		{name: "the database path with a trailing slash", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT ROW"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "topic", Name: "/local/"}},
			want: "a YDB grant requires an exact object path"},
		{name: "a resource pool in a directory", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool", Name: "ops/batch"}},
			want: "a resource pool belongs to the database and is named by its name alone"},
		{name: "a resource pool path outside the pools", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool", Name: "/local/ops/batch"}},
			want: "a resource pool belongs to the database and is named by its name alone"},
		{name: "the pools directory", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool", Name: "/local/.metadata/workload_manager/pools/"}},
			want: "a YDB grant requires an exact object path"},
		{name: "no resource pool name", grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"USE"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool"}},
			want: "a YDB grant requires an exact object path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := commonobject.GrantRefusal(test.grant)
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, test.want)
		})
	}
}

// TestDeclaredRoleRefusal_FailurePath pins the declared users and groups YDB
// cannot hold: a group that logs in or declares a password, whose value never
// reaches the declaration, and an empty group in member_of. Ported from the
// host's TestValidateDeclared_FailurePath.
func TestDeclaredRoleRefusal_FailurePath(t *testing.T) {
	tests := []struct {
		name string
		role *protocol.SQLRole
		want string
	}{
		{name: "a group that logs in", role: &protocol.SQLRole{Name: "readers", Group: true, Login: true},
			want: `principal "readers" declares login, which a YDB user or group does not carry; a group never logs in`},
		{name: "a group with a password", role: &protocol.SQLRole{Name: "readers", Group: true, PasswordDeclared: true},
			want: `principal "readers" declares a password, which a YDB user or group does not carry; a group never logs in`},
		{name: "an empty group", role: &protocol.SQLRole{Name: "app", MemberOf: []string{" "}},
			want: `principal "app" names an empty group`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := commonobject.DeclaredRoleRefusal(test.role, []*protocol.SQLRole{test.role})
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, test.want)
		})
	}
}

// TestDeclaredRoleRefusalNamesEveryProblem_FailurePath pins that a declared principal
// with several mistakes is refused once, naming each: an attribute YDB does
// not carry, and every member_of entry YDB cannot hold. Ported from the
// host's TestValidateDeclared_ReportsEveryProblem.
func TestDeclaredRoleRefusalNamesEveryProblem_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	role := &protocol.SQLRole{Name: "readers", Group: true, Login: true, MemberOf: []string{" ", "readers", "app"}}
	err := commonobject.DeclaredRoleRefusal(role, []*protocol.SQLRole{role, {Name: "app"}})
	c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
	c.Assert(err.Error(), qt.Equals, `unsupported feature: principal "readers" declares login, which a YDB user or group does not carry; a group never logs in`+"\n"+
		`unsupported feature: principal "readers" names an empty group`+"\n"+
		`unsupported feature: principal "readers" names itself in member_of`+"\n"+
		`unsupported feature: principal "readers" is a member of "app", which the schema declares as a user`)
}
