//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// accessNames are the principals one run creates: a group, a user that logs
// in, and a user that is blocked. Users and groups belong to the database
// rather than to a directory, so each run takes names of its own.
type accessNames struct {
	group, user, blocked string
}

func newAccessNames() accessNames {
	suffix := strings.ToLower(rand.Text())[:8]
	return accessNames{group: "ptahaccg" + suffix, user: "ptahaccu" + suffix, blocked: "ptahaccb" + suffix}
}

func (n accessNames) all() []string { return []string{n.group, n.user, n.blocked} }

// removeAccess revokes what the run's principals hold on the database, and
// drops them. DROP USER and DROP GROUP leave a principal's entries behind, and
// the next principal of that name would inherit them. The entries on the
// fixture directory and on what it holds go with the directory.
func removeAccess(c *qt.C, opened *connection.Connection, database string, names accessNames) {
	c.Helper()
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, principal := range names.all() {
			// A principal that holds nothing, or was never created, has nothing to take.
			_ = execFixtureSchema(ctx, opened.DB, "REVOKE ALL ON `"+database+"` FROM `"+principal+"`")
		}
		for _, statement := range []string{"DROP USER IF EXISTS `" + names.user + "`", "DROP USER IF EXISTS `" + names.blocked + "`", "DROP GROUP IF EXISTS `" + names.group + "`"} {
			c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
}

// accessTable declares the table the grants name, alone.
func accessTable(directory string) string {
	return "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      note: {type: Utf8}\n"
}

// accessDeclaration is the host's fixture as YAML: the table, a group, a user
// that logs in with a password and is a member of userGroups, a blocked user
// in the group, and grants on the table, the directory and the database.
func accessDeclaration(directory string, names accessNames, userGroups []string, blockedLogin bool, tablePrivileges ...string) string {
	return accessTable(directory) +
		"roles:\n  " + names.group + ":\n    group: true\n" +
		"  " + names.user + ":\n    login: true\n    member_of: [" + strings.Join(userGroups, ", ") + "]\n" +
		"  " + names.blocked + ":\n    login: " + fmt.Sprint(blockedLogin) + "\n    member_of: [" + names.group + "]\n" +
		"grants:\n  table:\n    role: " + names.group + "\n    privileges: [" + strings.Join(tablePrivileges, ", ") + "]\n    on_table: " + directory + ".orders\n" +
		"  directory:\n    role: " + names.group + "\n    privileges: [LIST]\n    on_schema: " + directory + "\n" +
		"  database:\n    role: " + names.user + "\n    privileges: [CONNECT]\n    on_database: true\n"
}

// roleEntry is one principal a read reports.
type roleEntry struct {
	Name         string
	Login, Group bool
}

// membershipEntry is one membership a read reports.
type membershipEntry struct{ Group, Member string }

// principalsOf is what a read of directory reports of the run's principals:
// the principals, the memberships that make one of them a member, other than
// in the group of all users every user is in, and the permissions they hold
// on the database, a directory and a table.
func principalsOf(c *qt.C, client *providerruntime.Client, session *protocol.Session, directory string, names accessNames) ([]roleEntry, []membershipEntry, []grantEntry) {
	c.Helper()
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	var roles []roleEntry
	for _, role := range snapshot.GetCommonObjects().GetRoles() {
		if slices.Contains(names.all(), role.GetName()) {
			roles = append(roles, roleEntry{role.GetName(), role.GetLogin(), role.GetGroup()})
		}
	}
	slices.SortFunc(roles, func(a, b roleEntry) int { return strings.Compare(a.Name, b.Name) })
	var memberships []membershipEntry
	for _, membership := range snapshot.GetCommonObjects().GetMemberships() {
		if slices.Contains(names.all(), membership.GetMember()) && membership.GetGroup() != "USERS" {
			memberships = append(memberships, membershipEntry{membership.GetGroup(), membership.GetMember()})
		}
	}
	slices.SortFunc(memberships, func(a, b membershipEntry) int { return strings.Compare(a.Group+" "+a.Member, b.Group+" "+b.Member) })
	return roles, memberships, grantsIn(c, client, session, directory, []string{"database", "schema", "table"}, names.all())
}

// loginError is why the user cannot open a session with password, or nil
// when it can.
func loginError(c *qt.C, user, password string) error {
	c.Helper()
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	parsed.User = url.UserPassword(user, password)
	address, err := connection.Parse(parsed.String())
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	if err != nil {
		return err
	}
	return opened.Close()
}

// Ported from the host's TestYDBAccessControl_RoundTrip: the host applies
// users, a group, memberships, one in the cluster's DATA-READERS, and grants
// on a table, a directory and the database through this provider; the read
// reports them as declared and nothing is left to plan, and a second apply
// plans nothing either. A change of a membership, of a user's login and of a
// grant is planned in place as exactly four statements, and nothing is left
// to plan after it. The table is created in the same run as the access, as
// the host's embedded engine plans it: a grant on the table reads the table and
// changes its access, so the provider's creation stays the table's only writer.
func TestHostAppliesAccessControl_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "orders")+"`"), qt.IsNil)
	})
	declared := accessDeclaration(directory, names, []string{"DATA-READERS", names.group}, false, "SELECT ROW", "ydb.generic.list")
	changed := accessDeclaration(directory, names, []string{"DATA-READERS"}, true, "SELECT ROW", "UPDATE ROW")

	hostRun(c, directory, "the table and the access", declared, `(?s).*Schema apply completed successfully.*`)
	settled := hostPlanned(c, directory, "yaml", declared)
	hostRun(c, directory, "a second apply", declared, `(?s).*Schema is synced, no changes to be made.*`)
	roles, memberships, grants := principalsOf(c, client, session, directory, names)
	changes := hostPlanned(c, directory, "yaml", changed)
	hostRun(c, directory, "the change", changed, `(?s).*Schema apply completed successfully.*`)

	c.Assert(settled, qt.HasLen, 0)
	c.Assert(roles, qt.DeepEquals, []roleEntry{{Name: names.blocked}, {Name: names.group, Group: true}, {Name: names.user, Login: true}})
	c.Assert(memberships, qt.DeepEquals, []membershipEntry{{"DATA-READERS", names.user}, {names.group, names.blocked}, {names.group, names.user}})
	c.Assert(grants, qt.DeepEquals, []grantEntry{
		{Role: names.user, Privilege: "ydb.database.connect", Kind: "database"},
		{Role: names.group, Privilege: "ydb.generic.list", Kind: "schema", Name: directory},
		{Role: names.group, Privilege: "ydb.generic.list", Kind: "table", Name: directory + "/orders"},
		{Role: names.group, Privilege: "ydb.granular.select_row", Kind: "table", Name: directory + "/orders"},
	})
	c.Assert(slices.Sorted(slices.Values(changes)), qt.DeepEquals, []string{
		"ALTER GROUP `" + names.group + "` DROP USER `" + names.user + "`",
		"ALTER USER `" + names.blocked + "` LOGIN",
		"GRANT 'ydb.granular.update_row' ON `" + directory + "/orders` TO `" + names.group + "`",
		"REVOKE 'ydb.generic.list' ON `" + directory + "/orders` FROM `" + names.group + "`",
	})
	c.Assert(hostPlanned(c, directory, "yaml", changed), qt.HasLen, 0)
}

// A user's password is set when the user is created and never read back, as
// the host's embedded engine sets it: the user logs in with the declared
// password and not with another, and a declaration that names another
// password plans nothing, so the first one still opens a session. The plan
// that creates the user is one CREATE USER, as the host's engine writes it:
// the statement names the variable of the password, never the password
// (ADR 0071), and the apply that plans from the schema needs no variable set.
func TestHostSetsAUserPasswordOnce_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.PasswordInputsFeature)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	first, second := "First1"+rand.Text(), "Second1"+rand.Text()
	user := func(password string) string {
		return "roles:\n  " + names.user + ":\n    login: true\n    password: \"" + password + "\"\n"
	}

	created := hostPlanned(c, directory, "yaml", user(first))
	hostRun(c, directory, "the user", user(first), `(?s).*Schema apply completed successfully.*`)
	declaredLogin, otherLogin := loginError(c, names.user, first), loginError(c, names.user, second)
	changed := hostPlanned(c, directory, "yaml", user(second))

	c.Assert(created, qt.HasLen, 1)
	c.Assert(created[0], qt.Equals, "CREATE USER `"+names.user+"` PASSWORD $"+passwordVariable(names.user))
	c.Assert(declaredLogin, qt.IsNil)
	c.Assert(otherLogin, qt.IsNotNil)
	c.Assert(changed, qt.HasLen, 0)
	c.Assert(loginError(c, names.user, first), qt.IsNil)
}

// A password in a YQL schema file has no home on the provider wire: a SQL
// schema reading carries no password to the host, which would create the user
// without one. The provider refuses the statement by the user's name, and no
// output repeats the password. The host's embedded engine reads it and sets
// it once; the parity gap is the wire's.
func TestHostRefusesAPasswordInAYQLSchema_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	canary := "Canary1" + rand.Text()

	output, err := hostApplySource(c, directory, "sql", "CREATE USER `"+names.user+"` PASSWORD '"+canary+"';")
	login := loginError(c, names.user, canary)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "user `"+names.user+"` declares a password, which a SQL schema reading has no home for")
	c.Assert(output, qt.Not(qt.Contains), canary)
	c.Assert(login, qt.IsNotNil)
}

// Ported from the host's TestYDBReader_LeavesTheDatabaseOwnerOutOfTheDescription:
// a read of a database owned by one of its users, local-ydb's root, records
// the owner as the database's, able to log in, and leaves it out of the
// principals the read describes. A description is what a replay creates, and
// the owner exists before any migration runs.
func TestReadLeavesTheDatabaseOwnerOutOfThePrincipals_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, database := inspectionFixture(c)
	client, session := accessSession(c)

	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})

	c.Assert(err, qt.IsNil)
	objects := snapshot.GetCommonObjects()
	owner := slices.IndexFunc(objects.GetObjectOwners(), func(o *protocol.ObjectOwner) bool { return o.GetObject().GetKind() == "database" })
	c.Assert(owner >= 0, qt.IsTrue)
	c.Assert([]any{objects.ObjectOwners[owner].GetObject().GetDatabase(), objects.ObjectOwners[owner].GetOwner(), objects.ObjectOwners[owner].OwnerCanLogin},
		qt.DeepEquals, []any{database, "root", new(true)})
	c.Assert(slices.ContainsFunc(objects.GetRoles(), func(role *protocol.ObservedRole) bool { return role.GetName() == "root" }), qt.IsFalse)
	c.Assert(objects.GetRolesOutOfScope(), qt.Contains, "root")
}

// Ported from the host's TestYDBDesiredYQL_Principals: users and groups are
// database-wide. YQL declares users, one that may not log in, and a group
// with both as members; then logs the blocked user in and takes the other
// out of the group. Each source plans a change and converges, and an empty
// file keeps every principal the database holds, since it describes none.
// The host's sources give the first user a password, which a SQL schema
// reading cannot carry on the provider wire; the user is declared without
// one here (see TestHostRefusesAPasswordInAYQLSchema_FailurePath).
func TestHostConvergesYQLPrincipals_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	for _, source := range []string{
		fmt.Sprintf("CREATE USER `%s`; CREATE USER %s NOLOGIN; CREATE GROUP %s WITH USER %s, %s;", names.user, names.blocked, names.group, names.user, names.blocked),
		fmt.Sprintf("CREATE USER %s; CREATE USER %s NOLOGIN; CREATE GROUP %s WITH USER %s, %s; ALTER USER %s WITH LOGIN; ALTER GROUP %s DROP USER %s;",
			names.user, names.blocked, names.group, names.user, names.blocked, names.blocked, names.group, names.user),
	} {
		planned := hostPlanned(c, directory, "sql", source)
		output, err := hostApplySource(c, directory, "sql", source)

		c.Assert(planned, qt.Not(qt.HasLen), 0, qt.Commentf("%s", source))
		c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
		c.Assert(hostPlanned(c, directory, "sql", source), qt.HasLen, 0, qt.Commentf("%s", source))
	}

	empty := hostPlanned(c, directory, "sql", "")
	roles, memberships, _ := principalsOf(c, client, session, directory, names)

	c.Assert(empty, qt.HasLen, 0)
	c.Assert(roles, qt.HasLen, 3)
	c.Assert(memberships, qt.DeepEquals, []membershipEntry{{names.group, names.blocked}})
}

// Ported from the host's TestYDBDesiredYQL_Privileges: YQL permissions on a
// table, a directory and the database converge. The grant option is a
// permission of its own in YDB, ydb.access.grant, and REVOKE GRANT OPTION FOR
// takes it together with the permission it names. Each source plans a change
// and converges, and the last, which declares no grant, leaves the
// principals with none.
//
// The table is applied first: the host refuses a common grant on a table the
// same plan creates through a provider, which its embedded engine plans in one
// run.
func TestHostConvergesYQLPrivileges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "orders")+"`"), qt.IsNil)
	})
	table := "`" + directory + "/orders`"
	created := "CREATE TABLE " + table + " (id Int64 NOT NULL, PRIMARY KEY(id));"
	prefix := created + fmt.Sprintf(" CREATE GROUP %s; CREATE USER %s;", names.group, names.user)
	first := prefix + fmt.Sprintf(" GRANT SELECT ROW ON %s TO %s WITH GRANT OPTION; GRANT LIST ON `%s` TO %s; GRANT CONNECT ON `%s` TO %s;",
		table, names.group, path.Join(database, directory), names.group, database, names.user)
	second := first + fmt.Sprintf(" REVOKE GRANT OPTION FOR SELECT ROW ON %s FROM %s; GRANT UPDATE ROW ON %s TO %s;", table, names.group, table, names.group)
	output, err := hostApplySource(c, directory, "sql", created)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	for _, source := range []string{first, second, prefix} {
		planned := hostPlanned(c, directory, "sql", source)
		output, err := hostApplySource(c, directory, "sql", source)

		c.Assert(planned, qt.Not(qt.HasLen), 0, qt.Commentf("%s", source))
		c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
		c.Assert(hostPlanned(c, directory, "sql", source), qt.HasLen, 0, qt.Commentf("%s", source))
	}

	roles, _, grants := principalsOf(c, client, session, directory, names)
	c.Assert(roles, qt.HasLen, 2)
	c.Assert(grants, qt.HasLen, 0)
}

// A grant on a table the same run alters is planned with the alteration: the
// grant follows the provider's step that changes the table's settings and adds
// a column, and the read reports all of it.
func TestHostGrantsOnATableTheRunAlters_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "orders")+"`"), qt.IsNil)
	})
	declared := accessDeclaration(directory, names, []string{names.group}, false, "SELECT ROW")
	altered := strings.Replace(declared, "    columns:\n", "    auto_partitioning_min_partitions_count: 2\n    columns:\n", 1)
	altered = strings.Replace(altered, "      note: {type: Utf8}\n", "      note: {type: Utf8}\n      total: {type: Int64}\n", 1)
	c.Assert(strings.Count(altered, "\n")-strings.Count(declared, "\n"), qt.Equals, 2)

	hostRun(c, directory, "the table", accessTable(directory), `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a column and the access", altered, `(?s).*Schema apply completed successfully.*`)
	hostRun(c, directory, "a second apply", altered, `(?s).*Schema is synced, no changes to be made.*`)
	_, _, grants := principalsOf(c, client, session, directory, names)
	c.Assert(grants, qt.Contains, grantEntry{Role: names.group, Privilege: "ydb.granular.select_row", Kind: "table", Name: directory + "/orders"})
	table, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(table.Close(), qt.IsNil) })
	described, err := table.Describe(c.Context(), path.Join(database, directory, "orders"))
	c.Assert(err, qt.IsNil)
	var columns []string
	for _, column := range described.GetColumns() {
		columns = append(columns, column.GetName())
	}
	c.Assert(columns, qt.DeepEquals, []string{"id", "note", "total"})
	c.Assert(described.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(2))
}
