//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// requireHostAsksForViewQueries skips a test on a host that does not ask this
// provider about view queries. schema.view-query-equivalence is the
// provider's alone, so a host that predates the call still sees it listed;
// the host adopted the call together with the two-way features of contract
// 25.33, so negotiating render.omissions is what tells the two apart.
func requireHostAsksForViewQueries(c *qt.C) {
	c.Helper()
	requireHostNegotiates(c, provider.RenderOmissionsFeature)
}

// viewWithAlias declares, in YQL, table t of directory and view v over it,
// whose query names its one column alias.
func viewWithAlias(directory, alias string) string {
	return "CREATE TABLE `" + directory + "/t` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
		"CREATE VIEW `" + directory + "/v` WITH (security_invoker = TRUE) AS SELECT id AS " + alias + " FROM `" + directory + "/t`;\n"
}

// A view whose declared query changes only the case of a name is replaced:
// YDB names keep case, so `AS answer` and `AS Answer` name two columns, and
// the provider compares view queries in the form the server stores them, as
// the host's embedded engine did. The host's own reading folds case and
// plans nothing for the change (G204). Measured with built-in 74de0b65b on
// 25.1.4.7 and 26.2.1.14: DROP VIEW and CREATE VIEW, and the next run plans
// nothing.
func TestHostReplacesAViewWhoseQueryOnlyChangesCase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostAsksForViewQueries(c)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	applyRuns(c, directory, "sql", viewWithAlias(directory, "Answer"))

	planned := hostPlanned(c, directory, "sql", viewWithAlias(directory, "answer"))
	applyRuns(c, directory, "sql", viewWithAlias(directory, "answer"))

	c.Assert(planned, qt.DeepEquals, []string{
		"DROP VIEW `" + directory + "/v`",
		"CREATE VIEW `" + directory + "/v` WITH (security_invoker = TRUE) AS",
	})
	c.Assert(storedView(c, opened, path.Join(database, directory, "v")), qt.Equals, "SELECT id AS answer FROM `"+directory+"/t`")
}
