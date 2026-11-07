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
		// The grant rows below are ported from the host's
		// TestRender_AccessStatements_FailurePath, TestValidateDeclared_FailurePath
		// and TestRenderSchema_DatabaseGrant_FailurePath. The capabilities name
		// neither external data sources nor secrets.
		{name: "a column grant", change: grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Columns: []string{"a"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "t"}}), want: "a YDB permission is on a whole object, not on columns"},
		{name: "a sequence", change: grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "sequence", Name: "s"}}), want: `a YDB grant cannot name object kind "sequence"`},
		{name: "a grant with the grant option", change: grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, WithGrantOption: true,
			Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "t"}}), want: "YDB stores the grant option as permission ydb.access.grant; grant or revoke that permission directly"},
		{name: "a revoke on an external table with external data sources off", change: revokeChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT ROW"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "external table", Schema: "ext", Name: "events"}}),
			want: "YDB access operation requires target capability external_data_sources"},
		{name: "a grant on a secret on a line without secrets", change: grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT ROW"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "secret", Schema: "ext", Name: "pw"}}), want: "YDB access operation requires target capability secrets"},
		{name: "the database with no path to name it by", change: grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"CONNECT"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "database"}}), want: "YDB takes this grant target only by its absolute database path; plan it against the database"},
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

func grantChange(grant *protocol.SQLGrant) *protocol.CommonObjectChange {
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: grant}}
}

func revokeChange(grant *protocol.SQLGrant) *protocol.CommonObjectChange {
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: grant}}
}

// Each access operation is refused on a target without the capability it
// needs, which the refusal names; every other capability is established.
// The rows past the first are ported from the host's
// TestRender_AccessStatements_RefusedWithoutTheKeys.
func TestRenderAccessRequiresAnEstablishedCapability(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  *protocol.CommonObjectChange
		without string
	}{
		{"a user", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app"}}}},
			"role_management"},
		{"a group", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "readers", Group: true}}}},
			"group_principals"},
		{"a membership", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AddMember{AddMember: &protocol.RoleMembership{Group: "readers", Member: "app"}}},
			"role_membership"},
		{"an untyped path cannot bypass database grant restrictions", grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"CONNECT"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "path", Name: "/local"}}), "database_grants"},
		{"a grant on the database", grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{"CONNECT"},
			Target: &protocol.Reference{Engine: "ydb", Kind: "database", Database: "/local"}}), "database_grants"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			capabilities := accessCapabilities()
			delete(capabilities, test.without)
			got, err := commonobject.Render(test.change, capabilities)
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, "requires target capability "+test.without)
			c.Assert(got, qt.Equals, "")
		})
	}
}

// ALL and ALL PRIVILEGES name YDB's full permission, as the host's embedded
// engine writes them. Ported from the host's TestRender_AccessStatements_HappyPath.
func TestRenderGrantOfAllIsTheFullPermission(t *testing.T) {
	for _, privilege := range []string{"ALL", "all privileges"} {
		t.Run(privilege, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(grantChange(&protocol.SQLGrant{Role: "app", Privileges: []string{privilege},
				Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "events"}}), accessCapabilities())
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, "GRANT 'ydb.generic.full' ON `events` TO `app`;")
		})
	}
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

// A grant on a secret is refused on a target without secrets, as 25.1 is,
// and written where the target has them. Ported from the host's
// TestRenderSQL_AccessNodes_FailurePath row "a grant on a secret on YDB 25.1".
func TestRenderGrantOnASecretRequiresSecrets_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"},
		Target: &protocol.Reference{Engine: "ydb", Kind: "secret", Schema: "ext", Name: "pw"}}}}
	got, err := commonobject.Render(change, accessCapabilities())
	c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
	c.Assert(err, qt.ErrorMatches, ".*YDB access operation requires target capability secrets")
	c.Assert(got, qt.Equals, "")
}

func TestRenderGrantOnASecret_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	caps := accessCapabilities()
	caps["secrets"] = true
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"},
		Target: &protocol.Reference{Engine: "ydb", Kind: "secret", Schema: "ext", Name: "pw"}}}}
	got, err := commonobject.Render(change, caps)
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.Equals, "GRANT 'ydb.generic.read' ON `ext/pw` TO `app`;")
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
		// Ported from the host's TestValidateDeclared_HoldsYDBToItsModel.
		{"the grant option", &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, WithGrantOption: true, Target: &protocol.Reference{Engine: "ydb", Kind: "table", Name: "items"}}},
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
