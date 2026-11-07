//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// removalVerdict is one assessment of a generation's safety report, without
// the statement and the subject, which name the test's own objects.
type removalVerdict struct {
	NodeType string `json:"node_type"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
}

// removalFixture creates, in a fixture directory, a table with a view over
// it and a grant to a group with a member, and returns the directory and a
// YAML declaration of the table, the group and the user alone.
func removalFixture(c *qt.C) (directory, declaration string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	group := hclGrantGroup(c, opened.DB)
	user := group + "u"
	table := path.Join(database, directory, "orders")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	for _, statement := range []string{
		"CREATE USER `" + user + "` PASSWORD 'p'",
		"ALTER GROUP `" + group + "` ADD USER `" + user + "`",
		"CREATE TABLE `" + table + "` (id Int64 NOT NULL, PRIMARY KEY (id))",
		"CREATE VIEW `" + path.Join(database, directory, "v") + "` WITH (security_invoker = TRUE) AS SELECT id FROM `" + table + "`",
		"GRANT 'ydb.generic.read' ON `" + table + "` TO `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	return directory, "tables:\n  orders:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"roles:\n  " + group + ":\n    group: true\n  " + user + ":\n    login: true\n"
}

// Ported from the YDB rows of the host's
// TestAssessRendered_RatesEachRemovalPerEngine and its role_memberships_removed
// replacement row: a generation that drops a view, revokes a grant and takes a
// member out of a group rates the DROP VIEW destructive and the other two
// warnings, with the host's reasons, in the safety report it writes beside
// the files. --check-destructive refuses that generation, whose one
// destructive statement is the DROP VIEW.
func TestHostRatesYDBRemovals_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory, declaration := removalFixture(c)

	migrations, output, err := hostGenerateOutput(c, directory, "yaml", declaration, "--report", "json")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	reports, err := filepath.Glob(filepath.Join(migrations, "*.safety.json"))
	c.Assert(err, qt.IsNil)
	c.Assert(reports, qt.HasLen, 1)
	content, err := os.ReadFile(reports[0])
	c.Assert(err, qt.IsNil)
	var report struct {
		Highest     string           `json:"highest"`
		Assessments []removalVerdict `json:"assessments"`
	}
	c.Assert(json.Unmarshal(content, &report), qt.IsNil, qt.Commentf("%s", content))
	_, checked, checkErr := hostGenerateOutput(c, directory, "yaml", declaration, "--check-destructive")

	c.Assert(report.Highest, qt.Equals, "destructive")
	c.Assert(report.Assessments, qt.DeepEquals, []removalVerdict{
		{"*ast.RevokePrivilegeNode", "warning", "REVOKE takes away access a role may rely on"},
		{"*ast.RevokeRoleMembershipNode", "warning", "removing a member from a role takes away the privileges it held through the role"},
		{"*ast.DropViewNode", "destructive", "DROP VIEW removes a definition the plan cannot restore, and every query that reads the view fails"},
	})
	c.Assert(checkErr, qt.IsNotNil)
	c.Assert(checked, qt.Contains, "destructive migration statements")
}
