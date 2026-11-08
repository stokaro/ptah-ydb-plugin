//go:build integration

package integration_test

import (
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// assertPlanPrecedes requires both operations in a host plan, with the drop
// before the creation that takes the freed YDB path.
func assertPlanPrecedes(c *qt.C, plan []string, drop, create string) {
	c.Helper()
	before, after := slices.Index(plan, drop), slices.Index(plan, create)
	c.Assert(before, qt.Not(qt.Equals), -1, qt.Commentf("plan: %v", plan))
	c.Assert(after, qt.Not(qt.Equals), -1, qt.Commentf("plan: %v", plan))
	c.Assert(before < after, qt.IsTrue, qt.Commentf("plan: %v", plan))
}

// The former host TestGenerateMigrationAST_OwnersTradeAPath requires each
// native owner to release a path before another owner's object takes it.
func TestHostNativeOwnersTradePaths(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE SECRET `"+absolute("was_secret")+"` WITH (value = 'probe');"), qt.IsNil)
	createSchemaObject(c, opened, "CREATE TOPIC `"+absolute("was_topic")+"`", "DROP TOPIC `"+absolute("was_topic")+"`")
	createNode(c, opened, absolute("was_node"))

	source := "coordination_nodes:\n  was_secret:\n    schema: " + directory +
		"\nsecrets:\n  was_topic:\n    schema: " + directory + "\n    value_env: " + variable +
		"\ntopics:\n  was_node:\n    schema: " + directory + "\n"
	plan := hostPlanned(c, directory, "yaml", source)
	c.Assert(plan, qt.HasLen, 6)
	unit := func(kind, name string) string { return kind + "-operation " + directory + "." + name }
	assertPlanPrecedes(c, plan, unit("secret", "was_secret"), unit("coordination-node", "was_secret"))
	assertPlanPrecedes(c, plan, unit("topic", "was_topic"), unit("secret", "was_topic"))
	assertPlanPrecedes(c, plan, unit("coordination-node", "was_node"), unit("topic", "was_node"))
}

// The former host TestCoordinationFeaturesTradePathsWithTopics requires both
// drops to precede the creations at the paths the other owner held.
func TestHostCoordinationNodesAndTopicsTradePaths(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	createNode(c, opened, absolute("was_node"))
	createSchemaObject(c, opened, "CREATE TOPIC `"+absolute("was_topic")+"`", "DROP TOPIC `"+absolute("was_topic")+"`")

	source := "coordination_nodes:\n  was_topic:\n    schema: " + directory +
		"\ntopics:\n  was_node:\n    schema: " + directory + "\n"
	plan := hostPlanned(c, directory, "yaml", source)
	c.Assert(plan, qt.HasLen, 4)
	unit := func(kind, name string) string { return kind + "-operation " + directory + "." + name }
	assertPlanPrecedes(c, plan, unit("coordination-node", "was_node"), unit("topic", "was_node"))
	assertPlanPrecedes(c, plan, unit("topic", "was_topic"), unit("coordination-node", "was_topic"))
}
