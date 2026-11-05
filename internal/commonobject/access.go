package commonobject

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// RoleRefusal checks the attributes YDB users and groups can carry. Password
// values never enter this diagnostic; PasswordDeclared records intent alone.
func RoleRefusal(role *protocol.SQLRole) error {
	if err := ydbacl.CheckName(role.GetName()); err != nil {
		return unsupported("principal %q: %s", role.GetName(), err)
	}
	var rejected []string
	for _, attribute := range []struct {
		name string
		set  bool
	}{
		{"superuser", role.GetSuperuser()}, {"createdb", role.GetCreateDb()},
		{"createrole", role.GetCreateRole()}, {"replication", role.GetReplication()},
		{"login", role.GetGroup() && role.GetLogin()},
		{"a password", role.GetGroup() && role.GetPasswordDeclared()},
	} {
		if attribute.set {
			rejected = append(rejected, attribute.name)
		}
	}
	if len(rejected) != 0 {
		return unsupported("principal %q declares %s, which a YDB user or group does not carry; a group never logs in",
			role.GetName(), strings.Join(rejected, ", "))
	}
	return nil
}

func accessCapability(capabilities map[string]bool, key string) error {
	if !capabilities[key] {
		return unsupported("YDB access operation requires target capability %s", key)
	}
	return nil
}

func renderAccess(change *protocol.CommonObjectChange, capabilities map[string]bool) (string, error) {
	if err := accessCapability(capabilities, "role_management"); err != nil {
		return "", err
	}
	switch operation := change.GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateRole:
		return createRole(operation.CreateRole, capabilities)
	case *protocol.CommonObjectChange_AlterRole:
		return alterRole(operation.AlterRole)
	case *protocol.CommonObjectChange_DropRole:
		role := operation.DropRole
		if err := ydbacl.CheckName(role.GetName()); err != nil {
			return "", unsupported("drop principal: %s", err)
		}
		kind := "USER"
		if role.GetKind() == "group" {
			if err := accessCapability(capabilities, "group_principals"); err != nil {
				return "", err
			}
			kind = "GROUP"
		} else if role.GetKind() != "role" {
			return "", unsupported("a YDB principal is a user or a group")
		}
		return "DROP " + kind + " IF EXISTS " + ydbtype.QuoteIdentifier(role.GetName()) + ";", nil
	case *protocol.CommonObjectChange_AddMember:
		return membership(operation.AddMember, "ADD USER", capabilities)
	case *protocol.CommonObjectChange_RemoveMember:
		return membership(operation.RemoveMember, "DROP USER", capabilities)
	case *protocol.CommonObjectChange_Grant:
		return grant(operation.Grant, false, capabilities)
	case *protocol.CommonObjectChange_Revoke:
		return grant(operation.Revoke, true, capabilities)
	default:
		return "", unsupported("this change is not a YDB access operation")
	}
}

func createRole(creation *protocol.RoleCreation, capabilities map[string]bool) (string, error) {
	role := creation.GetRole()
	if err := RoleRefusal(role); err != nil {
		return "", err
	}
	if role.GetGroup() {
		if err := accessCapability(capabilities, "group_principals"); err != nil {
			return "", err
		}
		if creation.GetPassword() != "" || creation.GetPasswordInput() != nil {
			return "", unsupported("a YDB group cannot have a password")
		}
		return accessComment(role.GetComment()) + "CREATE GROUP " + ydbtype.QuoteIdentifier(role.GetName()) + ";", nil
	}
	statement := "CREATE USER " + ydbtype.QuoteIdentifier(role.GetName())
	if creation.GetPassword() != "" || creation.GetPasswordInput() != nil {
		clause, err := passwordClause(role.GetName(), creation.GetPassword(), creation.GetPasswordInput())
		if err != nil {
			return "", err
		}
		statement += " " + clause
	}
	if !role.GetLogin() {
		statement += " NOLOGIN"
	}
	return accessComment(role.GetComment()) + statement + ";", nil
}

func alterRole(alteration *protocol.RoleAlteration) (string, error) {
	role := alteration.GetDesired()
	if err := RoleRefusal(role); err != nil {
		return "", err
	}
	if role.GetGroup() {
		return "", unsupported("a YDB group has no login or password attributes")
	}
	var password, login string
	for _, attribute := range alteration.GetChanged() {
		switch attribute {
		case "password":
			clause, err := passwordClause(role.GetName(), alteration.GetPassword(), alteration.GetPasswordInput())
			if err != nil {
				return "", err
			}
			password = " " + clause
		case "login":
			login = " NOLOGIN"
			if role.GetLogin() {
				login = " LOGIN"
			}
		default:
			return "", unsupported("ALTER USER changes %s, which a YDB user does not carry", attribute)
		}
	}
	if password == "" && login == "" {
		return "", unsupported("ALTER USER changes no login or password attribute")
	}
	return accessComment(role.GetComment()) + "ALTER USER " + ydbtype.QuoteIdentifier(role.GetName()) + password + login + ";", nil
}

func membership(member *protocol.RoleMembership, clause string, capabilities map[string]bool) (string, error) {
	if err := accessCapability(capabilities, "role_membership"); err != nil {
		return "", err
	}
	if !accessName(member.GetGroup()) || !accessName(member.GetMember()) {
		return "", unsupported("a membership names a group and a member")
	}
	if clause == "ADD USER" && member.GetGroup() == member.GetMember() {
		return "", unsupported("a group cannot declare itself as a member")
	}
	return "ALTER GROUP " + ydbtype.QuoteIdentifier(member.GetGroup()) + " " + clause + " " + ydbtype.QuoteIdentifier(member.GetMember()) + ";", nil
}

func accessName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && !strings.ContainsRune(name, 0)
}

func accessComment(comment string) string {
	if folded := strings.Join(strings.Fields(comment), " "); folded != "" {
		return "-- " + folded + "\n"
	}
	return ""
}

// passwordClause sets the password of user from what the host sent. A
// declared password, or a password hash, arrives as an execution input
// (schema.password-inputs, ADR 0071), and the clause names the input where the
// password stands: `PASSWORD $<input>`, which YDB refuses as written, so a
// statement run without Ptah sets no other password. The execution writes the
// bound value there, with HASH for a hash (see secretvalue.Expand). A value on
// the wire comes from a host that does not negotiate the feature; it would be
// written into every plan and migration file that shows the statement, so it
// is refused, naming the user and never the value. Neither a value nor an
// input removes the password.
func passwordClause(user, value string, input *protocol.ExecutionInput) (string, error) {
	switch {
	case input != nil:
		if err := secrets.CheckValueEnv(input.GetName()); err != nil {
			return "", unsupported("the password input of user %q: %s", user, err)
		}
		return "PASSWORD " + secrets.Reference(input.GetName()), nil
	case value != "":
		return "", unsupported("user %q declares a password, and the host sends the password itself, which would write it "+
			"into the plan; use a Ptah release that lists %s", user, provider.PasswordInputsFeature)
	default:
		return "PASSWORD ''", nil
	}
}

func grant(declared *protocol.SQLGrant, revoke bool, capabilities map[string]bool) (string, error) {
	if err := GrantRefusal(declared); err != nil {
		return "", err
	}
	object, err := grantPath(declared.GetTarget(), capabilities)
	if err != nil {
		return "", err
	}
	var names []string
	for _, privilege := range declared.GetPrivileges() {
		permission, _ := ydbacl.Permission(privilege)
		names = append(names, ydbtype.StringLiteral(permission))
	}
	verb, direction := "GRANT", "TO"
	if revoke {
		verb, direction = "REVOKE", "FROM"
	}
	return accessComment(declared.GetComment()) + fmt.Sprintf("%s %s ON %s %s %s;", verb, strings.Join(names, ", "), ydbtype.QuoteIdentifier(object), direction, ydbtype.QuoteIdentifier(declared.GetRole())), nil
}

// GrantRefusal checks an authored permission without assuming a server version
// or database path. Rendering additionally checks the measured capabilities.
func GrantRefusal(declared *protocol.SQLGrant) error {
	if declared.GetWithGrantOption() {
		return unsupported("YDB stores the grant option as permission %s; grant or revoke that permission directly", ydbacl.GrantPermission)
	}
	if len(declared.GetColumns()) != 0 {
		return unsupported("a YDB permission is on a whole object, not on columns")
	}
	if !accessName(declared.GetRole()) {
		return unsupported("a YDB permission names a user or group")
	}
	if strings.EqualFold(declared.GetRole(), "PUBLIC") {
		return unsupported("YDB has no PUBLIC principal; grant to the cluster's all-users group instead")
	}
	for _, privilege := range declared.GetPrivileges() {
		if _, known := ydbacl.Permission(privilege); !known {
			return unsupported("%q is not a YDB permission", privilege)
		}
	}
	if len(declared.GetPrivileges()) == 0 {
		return unsupported("a YDB grant names no permission")
	}
	return grantTargetRefusal(declared.GetTarget())
}

// DeclaredRoleRefusal checks role attributes and membership targets together.
// A group outside the declaration may be supplied by the cluster.
func DeclaredRoleRefusal(role *protocol.SQLRole, roles []*protocol.SQLRole) error {
	problems := []error{RoleRefusal(role)}
	for _, group := range role.GetMemberOf() {
		switch {
		case !accessName(group):
			problems = append(problems, unsupported("principal %q names an empty group", role.GetName()))
		case group == role.GetName():
			problems = append(problems, unsupported("principal %q names itself in member_of", role.GetName()))
		case slices.ContainsFunc(roles, func(target *protocol.SQLRole) bool { return target.GetName() == group && !target.GetGroup() }):
			problems = append(problems, unsupported("principal %q is a member of %q, which the schema declares as a user", role.GetName(), group))
		}
	}
	return errors.Join(problems...)
}
