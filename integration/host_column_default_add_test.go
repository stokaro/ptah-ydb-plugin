//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the 26.1 row of the host's
// TestGenerateMigrationAST_ColumnChanges_HappyPath: a nullable column added
// with a default on a line that takes one in ADD COLUMN gets the default in
// the statement, written as an Int32 literal. Its other two rows, DROP NOT
// NULL on 25.1 and SET DEFAULT on 26.2, are rows of
// TestHostSchemaDiffPlansEachChangeOfATable_HappyPath, whose 26.1 row adds a
// NOT NULL Int64 column instead.
func TestHostAddsANullableColumnWithADefault_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	from := compatFile(c, c.TempDir(), "from.yaml", itemsTable(""))
	to := compatFile(c, c.TempDir(), "to.yaml", itemsTable("      qty: {type: Int32, default: '5'}\n"))

	out, err := hostCommand(c, "schema", "diff", "--from", from, "--to", to, "--dev-url", devOnlyURL, "--server-version", "26.1.1.6")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(previewStatements(out), qt.DeepEquals, []string{"ALTER TABLE `items` ADD COLUMN `qty` Int32 DEFAULT 5;"})
}
