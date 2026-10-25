package yqlschema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// sequenceName is the name of the sequence YDB keeps a Serial column's
// values in.
func sequenceName(column string) string { return "_serial_column_" + column }

// declaresSerial reports whether a declared column type is a Serial.
func declaresSerial(declared string) bool {
	mapping, err := ydbtype.Map(declared, ydbtype.Capabilities{ydbtype.SerialColumns: true})
	return err == nil && mapping.Serial
}

// checkSequence refuses the START and INCREMENT a Serial's sequence cannot
// take.
func checkSequence(start, increment string) error {
	if err := sequenceSetting("identity_start", start, "a start", "`Start value: 0 cannot be less than min value: 1`"); err != nil {
		return err
	}
	return sequenceSetting("identity_increment", increment, "an increment",
		"a negative increment is a parse error, and 0 answers `Increment must not be zero`")
}

func sequenceSetting(attribute, value, setting, measured string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s %q is not a whole number YDB's ALTER SEQUENCE takes", attribute, value)
	}
	if parsed < 1 {
		return fmt.Errorf("%s %d: a YDB Serial's sequence takes %s of 1 or more (measured: %s)", attribute, parsed, setting, measured)
	}
	return nil
}

// role is the principal a written name names, in the statement's document
// or an earlier one, or nil.
func (c *converter) role(written string) *Role {
	name := identifierPart(written)
	for _, reading := range c.readings() {
		for i := range reading.Roles {
			if reading.Roles[i].Name == name {
				return &reading.Roles[i]
			}
		}
	}
	return nil
}

func (c *converter) createRole(node *createRole) error {
	if c.role(node.name) != nil {
		return fmt.Errorf("%w: CREATE ROLE %q repeats a role already declared in this desired schema", errUnmodeled, identifierPart(node.name))
	}
	role := Role{Name: identifierPart(node.name), Login: node.login, Inherit: true, Group: node.group, span: c.statement, document: c.document}
	if node.password != nil {
		role.Password = *node.password
		if role.Password != "" {
			role.passwordAt, role.passwordDocument = new(c.statement), c.document
		}
	}
	c.file.Roles = append(c.file.Roles, role)
	return nil
}

func (c *converter) alterUser(node *alterUser) error {
	role := c.role(node.name)
	if role == nil || role.Group {
		return fmt.Errorf("%w: ALTER USER requires a user declared earlier in this desired schema", errUnmodeled)
	}
	for _, option := range node.options {
		if option.login != nil {
			role.Login = *option.login
			continue
		}
		// An absent password means unmanaged, not empty. Accepting a reset
		// would leave a live password unchanged while claiming the schema
		// applied it.
		if *option.password == "" {
			return fmt.Errorf("%w: ALTER USER cannot request an empty password in a desired schema; omitted passwords are unmanaged", errUnmodeled)
		}
		role.Password = *option.password
		role.passwordAt, role.passwordDocument = new(c.statement), c.document
	}
	return nil
}

func (c *converter) membership(node *membership) error {
	group := identifierPart(node.group)
	member := c.role(node.member)
	if member == nil {
		return fmt.Errorf("%w: a group membership requires a member declared earlier in this desired schema", errUnmodeled)
	}
	target := c.role(node.group)
	if !node.add && target == nil {
		return fmt.Errorf("%w: removing a membership requires a group declared earlier in this desired schema; memberships in undeclared groups are preserved", errUnmodeled)
	}
	if target != nil && !target.Group {
		return fmt.Errorf("%w: group membership target %q is declared as a user", errUnmodeled, group)
	}
	if group == member.Name {
		return fmt.Errorf("%w: a group cannot name itself as a member in a desired schema", errUnmodeled)
	}
	if node.add {
		if !slices.Contains(member.MemberOf, group) {
			member.MemberOf = append(member.MemberOf, group)
		}
	} else {
		member.MemberOf = slices.DeleteFunc(member.MemberOf, func(name string) bool { return name == group })
	}
	return nil
}

// privilege folds a GRANT or a REVOKE of YDB permissions into the grants
// the load declares.
func (c *converter) privilege(node *privilege) error {
	verb := "GRANT"
	if !node.grant {
		verb = "REVOKE"
	}
	role := node.role
	if role == "" || role != strings.TrimSpace(role) {
		return fmt.Errorf("%w: a YDB permission subject cannot be empty or have surrounding whitespace", errUnmodeled)
	}
	grant, err := c.privilegeTarget(node.path)
	if err != nil {
		return err
	}
	grant.Role = role
	grant.span, grant.document = c.statement, c.document
	for _, privilege := range node.permissions {
		permission, ok := ydbacl.Permission(privilege)
		if !ok {
			return fmt.Errorf("%w: unknown YDB permission %q", errUnmodeled, privilege)
		}
		grant.Privileges = append(grant.Privileges, permission)
	}
	grant.canonicalize()
	if verb == "GRANT" {
		foldGrants(c.file, nil, []Grant{grant})
	} else {
		foldGrants(c.file, []Grant{grant}, nil)
	}
	return nil
}

// canonicalize writes a grant as the host's model holds it: each privilege
// once, in capitals, and the names trimmed.
func (g *Grant) canonicalize() {
	seen := make(map[string]bool)
	privileges := make([]string, 0, len(g.Privileges))
	for _, privilege := range g.Privileges {
		trimmed := strings.TrimSpace(privilege)
		if trimmed == "" {
			continue
		}
		normalized := strings.ToUpper(trimmed)
		if !seen[normalized] {
			seen[normalized] = true
			privileges = append(privileges, normalized)
		}
	}
	g.Privileges = privileges
	g.Role = strings.TrimSpace(g.Role)
	g.OnTable = strings.TrimSpace(g.OnTable)
	g.OnSchema = strings.TrimSpace(g.OnSchema)
	g.OnObject = strings.TrimSpace(g.OnObject)
	g.ObjectKind = strings.Join(strings.Fields(strings.ToUpper(g.ObjectKind)), " ")
}

// targetKey is what two grants name one object by.
func (g Grant) targetKey() string {
	switch {
	case g.OnDatabase:
		return "DATABASE"
	case strings.TrimSpace(g.OnObject) != "":
		return strings.Join(strings.Fields(strings.ToUpper(g.ObjectKind)), " ") + " " + strings.TrimSpace(g.OnObject)
	case strings.TrimSpace(g.OnSchema) != "":
		return "SCHEMA " + strings.TrimSpace(g.OnSchema)
	default:
		return "TABLE " + strings.TrimSpace(g.OnTable)
	}
}

// foldGrants applies revoked and granted to reading in the order the host's
// model folds them: a revocation takes each permission out of the grants
// and records it once, and a grant takes the permissions it names out of
// the revocations.
func foldGrants(reading *Reading, revoked, granted []Grant) {
	for _, declared := range revoked {
		if len(declared.Privileges) == 0 {
			reading.RevokedGrants = append(reading.RevokedGrants, declared)
			continue
		}
		target := declared.targetKey()
		for _, privilege := range declared.Privileges {
			reading.Grants = withoutPrivilege(reading.Grants, declared.Role, target, privilege)
			if !slices.ContainsFunc(reading.RevokedGrants, func(held Grant) bool {
				return held.Role == declared.Role && held.targetKey() == target && firstPrivilege(held) == privilege
			}) {
				entry := declared
				entry.Privileges = []string{privilege}
				reading.RevokedGrants = append(reading.RevokedGrants, entry)
			}
		}
	}
	for _, grant := range granted {
		target := grant.targetKey()
		all := slices.Contains(grant.Privileges, "ALL")
		reading.RevokedGrants = slices.DeleteFunc(reading.RevokedGrants, func(held Grant) bool {
			return held.Role == grant.Role && held.targetKey() == target && (all || slices.Contains(grant.Privileges, firstPrivilege(held)))
		})
		reading.Grants = append(reading.Grants, grant)
	}
}

func withoutPrivilege(grants []Grant, role, target, privilege string) []Grant {
	kept := make([]Grant, 0, len(grants))
	for _, grant := range grants {
		if grant.Role == role && grant.targetKey() == target {
			grant.Privileges = slices.DeleteFunc(slices.Clone(grant.Privileges), func(name string) bool { return name == privilege })
			if len(grant.Privileges) == 0 {
				continue
			}
		}
		kept = append(kept, grant)
	}
	return kept
}

func firstPrivilege(grant Grant) string {
	if len(grant.Privileges) == 0 {
		return ""
	}
	return grant.Privileges[0]
}
