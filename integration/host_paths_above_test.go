//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// YDB needs every path above an object to be a directory, so an object the
// plan creates below a path whose occupant it drops can only follow that
// drop. Each test here declares such an object from YAML that describes its
// kind and the occupant's completely, so the occupant it leaves out is
// dropped, and reads the directory and the occupant's path, the two its plan
// touches.

// occupyWith returns what puts an object at an absolute path with statement,
// in which {abs} stands for the path.
func occupyWith(statement string) func(context.Context, *connection.Connection, string) error {
	return func(ctx context.Context, opened *connection.Connection, absolute string) error {
		return execFixtureSchema(ctx, opened.DB, strings.ReplaceAll(statement, "{abs}", absolute))
	}
}

// occupyWithANode puts a coordination node at an absolute path through the
// coordination service: the query service refuses CREATE COORDINATION NODE
// (26.2.1.14 answers `no viable alternative at input 'CREATE COORDINATION'`).
func occupyWithANode(ctx context.Context, opened *connection.Connection, absolute string) error {
	return opened.Driver.Coordination().CreateNode(ctx, absolute, coordination.NodeConfig{})
}

// Ported from the host's TestYDBSecrets_CreatedBeneathADroppedTable: a secret
// declared below the path of a table the source leaves out is created after
// the table's drop. The server takes both, the next run finds nothing to
// change, and the secret is where the source put it. 25.1 has no secrets.
func TestHostCreatesASecretBeneathADroppedTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireCapability(c, targetCapabilities(c), secrets.Capability)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	variable := "PTAH_SECRET_HOLDER_" + rand.Text()
	t.Setenv(variable, "holder-value")
	holder := path.Join(directory, "holder")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, holder)+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	declared := "secrets:\n  pw:\n    schema: " + holder + "\n    value_env: " + variable + "\n"
	selectors := directory + "," + holder

	planned := hostPlanned(c, selectors, "yaml", declared)
	applyRuns(c, selectors, "yaml", declared)

	c.Assert(planned, qt.DeepEquals, []string{"table-drop-operation " + directory + ".holder", "secret-operation " + holder + ".pw"})
	c.Assert(listedNames(c, opened, path.Join(database, holder)), qt.DeepEquals, []string{"pw"})
}

// Ported from the host's TestYDBSecrets_OwnersOrderAgainstThePathsAbove: an
// object created below a path whose occupant the plan drops follows the drop
// whichever owners plan the two, a table, a coordination node or a secret.
// The server takes both, and the next run finds nothing to change. 25.1 has
// no secrets, so only the table case runs there.
func TestHostCreatesBelowADroppedOccupantAfterItsDrop_HappyPath(t *testing.T) {
	variable := "PTAH_SECRET_OCCUPIED_" + rand.Text()
	t.Setenv(variable, "occupied-value")
	for _, test := range []struct {
		name, needs, declared string
		occupy                func(context.Context, *connection.Connection, string) error
		want                  []string
	}{
		{name: "a coordination node below a dropped table",
			occupy:   occupyWith("CREATE TABLE `{abs}` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))"),
			declared: "coordination_nodes:\n  lock:\n    schema: {occupied}\n",
			want:     []string{"table-drop-operation {dir}.occupied", "coordination-node-operation {occupied}.lock"}},
		{name: "a secret below a dropped coordination node", needs: secrets.Capability,
			occupy:   occupyWithANode,
			declared: "coordination_nodes: {}\nsecrets:\n  pw:\n    schema: {occupied}\n    value_env: " + variable + "\n",
			want:     []string{"coordination-node-operation {dir}.occupied", "secret-operation {occupied}.pw"}},
		{name: "a coordination node below a dropped secret", needs: secrets.Capability,
			occupy:   occupyWith("CREATE SECRET `{abs}` WITH (value = 'probe')"),
			declared: "secrets: {}\ncoordination_nodes:\n  lock:\n    schema: {occupied}\n",
			want:     []string{"secret-operation {dir}.occupied", "coordination-node-operation {occupied}.lock"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCapability(c, targetCapabilities(c), test.needs)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			occupied := path.Join(directory, "occupied")
			spell := strings.NewReplacer("{occupied}", occupied, "{dir}", directory).Replace
			c.Assert(test.occupy(c.Context(), opened, path.Join(database, occupied)), qt.IsNil)
			want := make([]string, len(test.want))
			for i, unit := range test.want {
				want[i] = spell(unit)
			}
			selectors := directory + "," + occupied

			planned := hostPlanned(c, selectors, "yaml", spell(test.declared))
			applyRuns(c, selectors, "yaml", spell(test.declared))

			c.Assert(planned, qt.DeepEquals, want)
		})
	}
}
