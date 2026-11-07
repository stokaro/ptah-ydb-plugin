//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// coveredUniqueAccounts declares accounts in directory with a named UNIQUE
// that covers a column and carries a comment, an unnamed UNIQUE, a column's
// own UNIQUE, and a UNIQUE over the key.
func coveredUniqueAccounts(directory string) string {
	return "package entities\n\n" +
		"//ptah:schema:constraint name=\"uq_tenant_login\" type=\"UNIQUE\" table=\"accounts\" columns=\"tenant,login\" include=\"email\" comment=\"One login per tenant\"\n" +
		"//ptah:schema:constraint type=\"UNIQUE\" table=\"accounts\" columns=\"login\"\n" +
		"//ptah:schema:constraint name=\"uq_id\" type=\"UNIQUE\" table=\"accounts\" columns=\"id\"\n" +
		"//ptah:schema:table name=\"accounts\" schema=\"" + directory + "\"\ntype Account struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" unique=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"email\" type=\"TEXT\" unique=\"true\"\n\tEmail string\n" +
		"\t//ptah:schema:field name=\"tenant\" type=\"BIGINT\"\n\tTenant int64\n" +
		"\t//ptah:schema:field name=\"login\" type=\"TEXT\"\n\tLogin string\n}\n"
}

// Ported from the host's TestUniqueConstraintsAsIndexesFor_HappyPath, the part
// the creation and comment ports did not hold: a UNIQUE that covers a column.
// YDB has no UNIQUE constraint, so each UNIQUE the declaration holds is kept
// as a global unique index under the name the renderer gives it: the named
// one with its covered column and its comment, the unnamed one and the
// column's own UNIQUE as <table>_<column>_key. The UNIQUE over the key, named
// or marked on the column, is folded into the primary key and gets no index.
// The next two runs compare each declared UNIQUE with the index YDB holds for
// it and find nothing to change.
func TestHostKeepsAUniqueWithItsCoveredColumns_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := accountsFixture(c)

	applyUntilSynced(c, directory, "go", coveredUniqueAccounts(directory))

	c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{
		"uq_tenant_login":    {{"tenant", "login"}, {"email"}},
		"accounts_login_key": {{"login"}, nil},
		"accounts_email_key": {{"email"}, nil},
	})
	c.Assert(heldAttributes(c, opened, absolute)["ptah.comment.index.uq_tenant_login"], qt.Equals, "One login per tenant")
}
