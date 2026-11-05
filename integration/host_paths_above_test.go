//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
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
		// The table service creates a table in place of the host's CREATE
		// TABLE, which keeps that step's place after the drop.
		{name: "a table below a dropped coordination node",
			occupy:   occupyWithANode,
			declared: "coordination_nodes: {}\ntables:\n  items:\n    schema: {occupied}\n    columns:\n      id: {type: Uint64, primary: true}\n",
			want:     []string{"coordination-node-operation {dir}.occupied", "row-table-create-operation {occupied}.items"}},
		{name: "a table below a dropped topic",
			occupy:   occupyWith("CREATE TOPIC `{abs}`"),
			declared: "topics: {}\ntables:\n  items:\n    schema: {occupied}\n    columns:\n      id: {type: Uint64, primary: true}\n",
			want:     []string{"topic-operation {dir}.occupied", "row-table-create-operation {occupied}.items"}},
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

// Ported from the rows of two owners in the host's
// TestGenerateMigrationAST_CreationOverAnEmptiedDirectory_FailurePath: a plan
// that drops an object below a path and creates an object at the path is
// refused before anything runs, in the host's words, also where two planning
// services plan the two. YDB keeps the directory after its last object is dropped, so the
// creation could never apply. Without the refusal the plan runs the drop and
// YDB refuses the creation after it, so the object below is lost. The object
// below stays, and nothing is created. 25.1 has no secrets, so the secret row
// runs on 26.2.
func TestHostRefusesACreationOverAnotherServicesDropBelow_FailurePath(t *testing.T) {
	variable := "PTAH_SECRET_OVER_" + rand.Text()
	t.Setenv(variable, "over-value")
	for _, test := range []struct {
		name, needs, below, selected, declared string
		occupy                                 func(context.Context, *connection.Connection, string) error
		kept                                   Ydb_Scheme.Entry_Type
	}{
		{name: "a coordination node over a dropped topic", below: "events", occupy: occupyWith("CREATE TOPIC `{abs}`"), kept: Ydb_Scheme.Entry_TOPIC,
			selected: "{dir},{dir}/occupied", declared: "topics: {}\ncoordination_nodes:\n  occupied:\n    schema: {dir}\n"},
		{name: "a topic over a dropped coordination node", below: "lock", occupy: occupyWithANode, kept: Ydb_Scheme.Entry_COORDINATION_NODE,
			selected: "{dir},{dir}/occupied", declared: "coordination_nodes: {}\ntopics:\n  occupied:\n    schema: {dir}\n    min_active_partitions: 1\n"},
		{name: "a secret over a coordination node two levels down", needs: secrets.Capability, below: "locks/lock", occupy: occupyWithANode,
			kept: Ydb_Scheme.Entry_COORDINATION_NODE, selected: "{dir},{dir}/occupied,{dir}/occupied/locks",
			declared: "coordination_nodes: {}\nsecrets:\n  occupied:\n    schema: {dir}\n    value_env: " + variable + "\n"},
		// The table service plans both halves of this one; the host's
		// embedded engine refused it in the same words.
		{name: "a table over a dropped table", below: "items", kept: Ydb_Scheme.Entry_TABLE, selected: "{dir},{dir}/occupied",
			occupy:   occupyWith("CREATE TABLE `{abs}` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))"),
			declared: "tables:\n  occupied:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCapability(c, targetCapabilities(c), test.needs)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			occupied := path.Join(directory, "occupied")
			below := path.Join(occupied, test.below)
			c.Assert(test.occupy(c.Context(), opened, path.Join(database, below)), qt.IsNil)
			spell := strings.NewReplacer("{dir}", directory).Replace

			output, err := hostApplySelected(c, spell(test.selected), "yaml", spell(test.declared))

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "this plan drops "+below+" and creates an object at "+occupied+", which is a directory in the database. "+
				"YDB keeps a directory after its last object is dropped, and YQL has no statement that removes one")
			c.Assert(heldEntries(c, opened, path.Join(database, path.Dir(below))), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{path.Base(below): test.kept})
			c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"occupied": Ydb_Scheme.Entry_DIRECTORY})
		})
	}
}
