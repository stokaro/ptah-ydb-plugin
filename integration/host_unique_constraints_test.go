//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// uniqueAccounts declares accounts in directory as the host's UNIQUE
// constraint tests do: a named table-level UNIQUE over tenant and login, a
// column's own UNIQUE on email, and a UNIQUE over the key, which folds into
// the key.
func uniqueAccounts(directory string) string {
	return "package entities\n\n//ptah:schema:constraint name=\"uq_accounts_tenant_login\" type=\"UNIQUE\" table=\"accounts\" columns=\"tenant,login\"\n" +
		"//ptah:schema:table name=\"accounts\" schema=\"" + directory + "\"\ntype Account struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" unique=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"email\" type=\"TEXT\" unique=\"true\"\n\tEmail string\n" +
		"\t//ptah:schema:field name=\"tenant\" type=\"BIGINT\"\n\tTenant int64\n" +
		"\t//ptah:schema:field name=\"login\" type=\"TEXT\"\n\tLogin string\n}\n"
}

// accountsFixture is the lock fixture with accounts dropped when the test
// ends, and the absolute path of accounts.
func accountsFixture(c *qt.C) (opened *connection.Connection, directory, absolute string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "accounts")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	return opened, directory, absolute
}

// The host's TestYDBUniqueConstraint_RoundTrip. YDB has no UNIQUE
// constraint, and the host reads each declared UNIQUE as the global unique
// index that holds the same rows, under the name its YDB renderer gives it.
// The table is created with the two indexes, a UNIQUE over the key folds into
// the key, and every later run is synced. The indexes hold rows the way the
// constraints would: a second row with the same email is refused, and two
// rows without one are not.
func TestHostUniqueConstraintsRoundTrip_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := accountsFixture(c)
	for _, run := range []struct{ name, want string }{
		{"creation", `(?s).*row-table-create-operation.*Schema apply completed successfully.*`},
		{"a second run", `(?s).*Schema is synced, no changes to be made.*`},
		{"a third run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, uniqueAccounts(directory))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{
		"accounts_email_key": {{"email"}, nil}, "uq_accounts_tenant_login": {{"tenant", "login"}, nil},
	})
	c.Assert(heldIndexKinds(c, opened, absolute), qt.DeepEquals, map[string]options.IndexType{
		"accounts_email_key": options.IndexTypeGlobalUnique, "uq_accounts_tenant_login": options.IndexTypeGlobalUnique,
	})

	c.Assert(execFixtureSchema(c.Context(), opened.DB, "INSERT INTO `"+absolute+"` (id, email) VALUES (1, 'a@example.com'u)"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "INSERT INTO `"+absolute+"` (id, email) VALUES (2, 'a@example.com'u)"), qt.IsNotNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "INSERT INTO `"+absolute+"` (id, email) VALUES (3, NULL), (4, NULL)"), qt.IsNil)
	var count uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute+"` WHERE email IS NULL").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(2))
}

// The host's TestYDBUniqueConstraint_MatchesAUniqueIndex. A table that holds
// the unique indexes of the declared UNIQUEs, built by hand under the names
// the host gives them, is the declared table: the run is synced.
func TestHostUniqueConstraintsMatchUniqueIndexes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := accountsFixture(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (`id` Int64 NOT NULL, `email` Utf8, `tenant` Int64, `login` Utf8, "+
		"PRIMARY KEY (`id`), INDEX `uq_accounts_tenant_login` GLOBAL UNIQUE SYNC ON (`tenant`, `login`), INDEX `accounts_email_key` GLOBAL UNIQUE SYNC ON (`email`))"), qt.IsNil)
	output, err := hostApply(c, directory, uniqueAccounts(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
}
