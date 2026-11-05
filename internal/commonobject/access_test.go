package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func accessCapabilities() map[string]bool {
	return map[string]bool{"role_management": true, "group_principals": true, "role_membership": true,
		"database_grants": true, "relative_grant_paths": true, "topics": true, "resource_pools": true}
}

func TestRenderAccess_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{name: "user", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app", Login: true}}}}, want: "CREATE USER `app`;"},
		{name: "disabled user", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app"}}}}, want: "CREATE USER `app` NOLOGIN;"},
		{name: "empty group", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "team", Group: true, MemberOf: []string{"DATA-READERS"}}}}}, want: "CREATE GROUP `team`;"},
		{name: "membership in built-in group", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AddMember{AddMember: &protocol.RoleMembership{Group: "DATA-READERS", Member: "app"}}}, want: "ALTER GROUP `DATA-READERS` ADD USER `app`;"},
		{name: "membership removal", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_RemoveMember{RemoveMember: &protocol.RoleMembership{Group: "team", Member: "app"}}}, want: "ALTER GROUP `team` DROP USER `app`;"},
		{name: "drop group", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropRole{DropRole: &protocol.Reference{Kind: "group", Name: "team"}}}, want: "DROP GROUP IF EXISTS `team`;"},
		{name: "enable login", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{Desired: &protocol.SQLRole{Name: "app", Login: true}, Changed: []string{"login"}}}}, want: "ALTER USER `app` LOGIN;"},
		{name: "remove password", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{Desired: &protocol.SQLRole{Name: "app"}, Changed: []string{"password"}}}}, want: "ALTER USER `app` PASSWORD '';"},
		{name: "fold comment", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "team", Group: true, Comment: "team\nDROP USER app;"}}}}, want: "-- team DROP USER app;\nCREATE GROUP `team`;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(test.change, accessCapabilities())
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func passwordInput(name string) *protocol.ExecutionInput {
	return &protocol.ExecutionInput{Source: protocol.ExecutionInput_ENVIRONMENT, Name: name}
}

// A password reaches the provider as an execution input, and the statement
// names the input where the password stands; the value is written there only
// when the statement runs (ADR 0071).
func TestRenderPasswordNamesItsInput_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{name: "a created user", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{
			Role: &protocol.SQLRole{Name: "app", Login: true, PasswordDeclared: true}, PasswordInput: passwordInput("PTAH_SECRET_PASSWORD_APP")}}},
			want: "CREATE USER `app` PASSWORD $PTAH_SECRET_PASSWORD_APP;"},
		{name: "a changed password beside the login", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{
			Desired: &protocol.SQLRole{Name: "app", PasswordDeclared: true}, Changed: []string{"password", "login"}, PasswordInput: passwordInput("PTAH_SECRET_PASSWORD_APP")}}},
			want: "ALTER USER `app` PASSWORD $PTAH_SECRET_PASSWORD_APP NOLOGIN;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(test.change, accessCapabilities())
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func TestRenderAccess_FailurePath(t *testing.T) {
	tests := []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{name: "reserved create name", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "ADMINS", Group: true}}}}, want: "lower-case ASCII"},
		{name: "unsupported user attribute", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app", Superuser: true}}}}, want: "superuser"},
		{name: "group password", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "team", Group: true}, Password: "must-not-leak"}}}, want: "cannot have a password"},
		{name: "unsupported alteration", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{Desired: &protocol.SQLRole{Name: "app"}, Changed: []string{"inherit"}, Password: "must-not-leak"}}}, want: "changes inherit"},
		{name: "a password value from a host without password inputs", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{
			Role: &protocol.SQLRole{Name: "app", Login: true, PasswordDeclared: true}, Password: "must-not-leak"}}},
			want: `user "app" declares a password, and the host sends the password itself, which would write it into the plan; use a Ptah release that lists schema.password-inputs`},
		{name: "a changed password value from such a host", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterRole{AlterRole: &protocol.RoleAlteration{
			Desired: &protocol.SQLRole{Name: "app", PasswordDeclared: true}, Changed: []string{"password"}, Password: "must-not-leak"}}}, want: "the host sends the password itself"},
		{name: "a password hash value from such a host", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{
			Role: &protocol.SQLRole{Name: "app", PasswordDeclared: true}, Password: `{"hash":"must-not-leak","salt":"s","type":"argon2id"}`}}}, want: "the host sends the password itself"},
		{name: "an input outside the secret prefix", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{
			Role: &protocol.SQLRole{Name: "app", PasswordDeclared: true}, PasswordInput: passwordInput("HOME")}}}, want: `the password input of user "app"`},
		{name: "a group's password input", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{
			Role: &protocol.SQLRole{Name: "team", Group: true}, PasswordInput: passwordInput("PTAH_SECRET_PASSWORD_TEAM")}}}, want: "cannot have a password"},
		{name: "empty membership", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AddMember{AddMember: &protocol.RoleMembership{Group: "team"}}}, want: "names a group and a member"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(test.change, accessCapabilities())
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, test.want)
			c.Assert(err.Error(), qt.Not(qt.Contains), "must-not-leak")
			c.Assert(got, qt.Equals, "")
		})
	}
}

func TestRenderAccessRequiresAnEstablishedCapability(t *testing.T) {
	c := testcheck.New(t)
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app"}}}}
	got, err := commonobject.Render(change, nil)
	c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
	c.Assert(err.Error(), qt.Contains, "role_management")
	c.Assert(got, qt.Equals, "")
}

func TestRenderGrant_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		target *protocol.Reference
		want   string
	}{
		{name: "table", target: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app/eu", Name: "items"}, want: "app/eu/items"},
		{name: "database", target: &protocol.Reference{Engine: "ydb", Kind: "database", Database: "/local"}, want: "/local"},
		{name: "directory", target: &protocol.Reference{Engine: "ydb", Kind: "schema", Name: "app/eu"}, want: "app/eu"},
		{name: "topic", target: &protocol.Reference{Engine: "ydb", Kind: "topic", Name: "events"}, want: "events"},
		{name: "resource pool", target: &protocol.Reference{Engine: "ydb", Kind: "resource pool", Name: "fast.pool"}, want: ".metadata/workload_manager/pools/fast.pool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT", "GRANT"}, Target: test.target}}}
			got, err := commonobject.Render(change, accessCapabilities())
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, "GRANT 'ydb.generic.read', 'ydb.access.grant' ON `"+test.want+"` TO `app`;")
		})
	}
}

func TestRenderGrantUsesTheDatabaseOnOlderLines(t *testing.T) {
	c := testcheck.New(t)
	caps := accessCapabilities()
	caps["relative_grant_paths"] = false
	target := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Name: "items"}
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: target}}}
	got, err := commonobject.Render(change, caps)
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.Equals, "REVOKE 'ydb.generic.read' ON `/local/items` FROM `app`;")
}

func TestRenderGrantRefusesAnUnboundDatabaseOnOlderLines(t *testing.T) {
	c := testcheck.New(t)
	caps := accessCapabilities()
	caps["relative_grant_paths"] = false
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: &protocol.SQLGrant{
		Role: "app", Privileges: []string{"SELECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "items"},
	}}}
	got, err := commonobject.Render(change, caps)
	c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
	c.Assert(err.Error(), qt.Contains, "absolute database path")
	c.Assert(got, qt.Equals, "")
}

func TestDeclaredAccessRefusesUnsupportedIntent(t *testing.T) {
	for _, test := range []struct {
		name  string
		grant *protocol.SQLGrant
	}{
		{"public", &protocol.SQLGrant{Role: "PUBLIC", Privileges: []string{"SELECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "items"}}},
		{"unknown permission", &protocol.SQLGrant{Role: "app", Privileges: []string{"MADE UP"}, Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "items"}}},
		{"escaping path", &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: "topic", Schema: "..", Name: "items"}}},
		{"qualified pool", &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: "resource pool", Name: "/other/pool"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(commonobject.GrantRefusal(test.grant), qt.ErrorIs, commonobject.ErrUnsupported)
		})
	}
}

func TestDeclaredMembershipTargetsAGroup(t *testing.T) {
	c := testcheck.New(t)
	user := &protocol.SQLRole{Name: "app", MemberOf: []string{"team"}}
	c.Assert(commonobject.DeclaredRoleRefusal(user, []*protocol.SQLRole{user}), qt.IsNil)
	c.Assert(commonobject.DeclaredRoleRefusal(user, []*protocol.SQLRole{user, {Name: "team", Group: true}}), qt.IsNil)
	c.Assert(commonobject.DeclaredRoleRefusal(user, []*protocol.SQLRole{user, {Name: "team"}}), qt.ErrorMatches, ".*declares as a user")
	user.MemberOf = []string{"app"}
	c.Assert(commonobject.DeclaredRoleRefusal(user, []*protocol.SQLRole{user}), qt.ErrorMatches, ".*names itself.*")
}

func TestPrepareCanonicalizesPermissionAliasesWithoutChangingSource(t *testing.T) {
	c := testcheck.New(t)
	declared := &protocol.DeclaredCommonObjects{Grants: []*protocol.SQLGrant{{Role: "app", Privileges: []string{"select row", "GRANT"},
		Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "items"}}}}
	prepared := commonobject.Prepare(declared)
	c.Assert(prepared.Grants[0].Privileges, qt.DeepEquals, []string{"ydb.granular.select_row", "ydb.access.grant"})
	c.Assert(declared.Grants[0].Privileges, qt.DeepEquals, []string{"select row", "GRANT"})
}
