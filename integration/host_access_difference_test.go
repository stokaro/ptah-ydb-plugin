//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// accessDifferenceNames are the principals of one access difference test.
type accessDifferenceNames struct{ readers, app, auditors, ops, someone string }

// newAccessDifferenceNames names the principals of a test, unique to it, and
// drops each when the test ends.
func newAccessDifferenceNames(c *qt.C, db *sql.DB) accessDifferenceNames {
	c.Helper()
	base := "ptahacl" + strings.ToLower(rand.Text())[:6]
	names := accessDifferenceNames{readers: base + "r", app: base + "a", auditors: base + "au", ops: base + "o", someone: base + "s"}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP USER IF EXISTS `" + names.app + "`", "DROP USER IF EXISTS `" + names.someone + "`",
			"DROP USER IF EXISTS `" + names.auditors + "`", "DROP GROUP IF EXISTS `" + names.readers + "`",
			"DROP GROUP IF EXISTS `" + names.auditors + "`", "DROP GROUP IF EXISTS `" + names.ops + "`"} {
			c.Check(execFixtureSchema(ctx, db, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	return names
}

// accessDifferenceDeclaration declares the table orders of directory, the
// group readers, the user app in it and in DATA-READERS, the group auditors,
// and grants on the table, the directory and the database, as the host's
// fixture does.
func accessDifferenceDeclaration(directory string, names accessDifferenceNames) string {
	return "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"roles:\n  " + names.readers + ":\n    group: true\n  " + names.app + ":\n    login: true\n    member_of: [" + names.readers + ", DATA-READERS]\n" +
		"  " + names.auditors + ":\n    group: true\n" +
		"grants:\n  table:\n    role: " + names.readers + "\n    privileges: [SELECT ROW, ydb.generic.list]\n    on_table: " + directory + ".orders\n" +
		"  directory:\n    role: " + names.readers + "\n    privileges: [LIST]\n    on_schema: " + directory + "\n" +
		"  database:\n    role: " + names.app + "\n    privileges: [CONNECT]\n    on_database: true\n"
}

// Ported from the host's TestCompare_YDBAccessControl_PlansTheDifference: after
// the declaration is applied, the database loses the user's membership in the
// declared group and the group's SELECT ROW, and gains a member of a declared
// group the declaration does not name, a grant to the declared user, a group
// the declaration does not declare with the user in it, and a grant to a user
// it does not declare. The plan is exactly what differs for the declared
// principals: the membership and the grant put back, and the membership and
// the grant revoked. A membership in a group the declaration does not
// declare is left alone, USERS included, which every new user joins, and so
// is a permission a principal the declaration does not declare holds.
func TestHostPlansTheAccessDifference_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	names := newAccessDifferenceNames(c, opened.DB)
	declared := accessDifferenceDeclaration(directory, names)
	applied, err := hostApplySource(c, directory, "yaml", declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	table := path.Join(database, directory, "orders")
	for _, statement := range []string{
		"ALTER GROUP `" + names.readers + "` DROP USER `" + names.app + "`",
		"ALTER GROUP `" + names.auditors + "` ADD USER `" + names.readers + "`",
		"CREATE GROUP `" + names.ops + "`",
		"ALTER GROUP `" + names.ops + "` ADD USER `" + names.app + "`",
		"CREATE USER `" + names.someone + "` PASSWORD 'p'",
		"GRANT 'ydb.granular.erase_row' ON `" + table + "` TO `" + names.someone + "`",
		"GRANT 'ydb.granular.erase_row' ON `" + table + "` TO `" + names.app + "`",
		"REVOKE 'ydb.granular.select_row' ON `" + table + "` FROM `" + names.readers + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	planned := hostPlanned(c, directory, "yaml", declared)

	relative := directory + "/orders"
	c.Assert(planned, qt.DeepEquals, []string{
		"REVOKE 'ydb.granular.erase_row' ON `" + relative + "` FROM `" + names.app + "`",
		"ALTER GROUP `" + names.auditors + "` DROP USER `" + names.readers + "`",
		"ALTER GROUP `" + names.readers + "` ADD USER `" + names.app + "`",
		"GRANT 'ydb.granular.select_row' ON `" + relative + "` TO `" + names.readers + "`",
	})
}

// The host's same test also changed a principal's kind: the declaration
// declares auditors as a group where the database holds a user of that name.
// YDB has no statement that turns one into the other, and the host refuses
// the plan, naming the principal, rather than planning a drop and a create
// that would lose what the principal holds.
func TestHostPlansTheAccessDifference_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	names := newAccessDifferenceNames(c, opened.DB)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE USER `"+names.auditors+"` PASSWORD 'p'"), qt.IsNil)

	out, err := hostApplySource(c, directory, "yaml", accessDifferenceDeclaration(directory, names))

	c.Assert(err, qt.IsNotNil)
	c.Assert(out, qt.Contains, "changing the group of role "+names.auditors)
}
