//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// heldNode reads every setting of the node at absolute back through the
// coordination service.
func heldNode(c *qt.C, opened *connection.Connection, absolute string) coordination.NodeConfig {
	c.Helper()
	_, config, err := opened.Driver.Coordination().DescribeNode(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	return *config
}

// coordinationJobs declares table jobs and the coordination nodes given, in
// YAML, in the fixture directory.
func coordinationJobs(directory, nodes string) string {
	return strings.ReplaceAll("tables:\n  jobs:\n    schema: {dir}\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"+
		"coordination_nodes:\n"+nodes, "{dir}", directory)
}

// Ported from the host's TestYDBCoordinationNodes_RoundTrip: the host creates
// a node with all five settings and one with none beside a table, finds
// nothing to change on the next two runs, and the coordination service holds
// the settings as declared. The plain node's settings written out at YDB's
// defaults describe the same node and plan nothing. A change names only the
// settings that differ, and the node keeps every other one. A node the
// declaration leaves out is dropped.
func TestHostAppliesCoordinationNodesWithEverySetting_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	limits := "  limits:\n    schema: {dir}\n    self_check_period: PT2.5S\n    session_grace_period: PT15S\n    read_consistency_mode: {read}\n" +
		"    attach_consistency_mode: relaxed\n    rate_limiter_counters_mode: detailed\n"
	plain := "  plain:\n    schema: {dir}\n"
	declared := coordinationJobs(directory, strings.ReplaceAll(limits, "{read}", "strict")+plain)

	applyUntilSynced(c, directory, "yaml", declared)
	c.Assert(heldNode(c, opened, path.Join(database, directory, "limits")), qt.DeepEquals, coordination.NodeConfig{
		SelfCheckPeriodMillis: 2500, SessionGracePeriodMillis: 15000, ReadConsistencyMode: coordination.ConsistencyModeStrict,
		AttachConsistencyMode: coordination.ConsistencyModeRelaxed, RatelimiterCountersMode: coordination.RatelimiterCountersModeDetailed,
	})
	atDefaults := "  plain:\n    schema: {dir}\n    self_check_period: PT1S\n    session_grace_period: PT10S\n    read_consistency_mode: relaxed\n" +
		"    attach_consistency_mode: strict\n    rate_limiter_counters_mode: aggregated\n"
	c.Assert(hostPlanned(c, directory, "yaml", coordinationJobs(directory, strings.ReplaceAll(limits, "{read}", "strict")+atDefaults)), qt.HasLen, 0)

	changed := strings.ReplaceAll(strings.ReplaceAll(limits, "{read}", "relaxed"), "PT2.5S", "PT3S")
	applyRuns(c, directory, "yaml", coordinationJobs(directory, changed+plain))
	c.Assert(heldNode(c, opened, path.Join(database, directory, "limits")), qt.DeepEquals, coordination.NodeConfig{
		SelfCheckPeriodMillis: 3000, SessionGracePeriodMillis: 15000, ReadConsistencyMode: coordination.ConsistencyModeRelaxed,
		AttachConsistencyMode: coordination.ConsistencyModeRelaxed, RatelimiterCountersMode: coordination.RatelimiterCountersModeDetailed,
	})

	applyRuns(c, directory, "yaml", coordinationJobs(directory, changed))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"jobs", "limits"})
}

// Ported from the host's TestYDBCoordinationNodes_TradeAPathWithATable: one
// run in which a table takes the path of a node the run drops and a node
// takes the path of a table the run drops. A path names one object, so YDB
// refuses a table at the node's path and a node at the table's path before the
// run, and only the plan's order, every drop before the creation that needs
// its path, lets it apply. The next run finds nothing to change.
func TestHostTradesAPathBetweenANodeAndATable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	toTable, toNode := path.Join(database, directory, "to_table"), path.Join(database, directory, "to_node")
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), toTable, coordination.NodeConfig{}), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+toNode+"` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))"), qt.IsNil)
	tableOverNode := execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+toTable+"` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))")
	nodeOverTable := opened.Driver.Coordination().CreateNode(c.Context(), toNode, coordination.NodeConfig{})
	declared := strings.ReplaceAll("tables:\n  to_table:\n    schema: {dir}\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"+
		"coordination_nodes:\n  to_node:\n    schema: {dir}\n    self_check_period: PT2S\n", "{dir}", directory)

	planned := hostPlanned(c, directory, "yaml", declared)
	applyRuns(c, directory, "yaml", declared)

	// 26.2.1.14 answers `Path is not a table or topic`, 25.1.4.7
	// `PathNotTable`.
	c.Assert(tableOverNode, qt.ErrorMatches, `(?s).*SCHEME_ERROR.*(Path is not a table or topic|PathNotTable).*`)
	c.Assert(nodeOverTable, qt.ErrorMatches, `(?s).*unexpected path type .*EPathTypeTable.*expected types: EPathTypeKesus.*`)
	c.Assert(slices.Index(planned, "coordination-node-operation "+directory+".to_table") <
		slices.Index(planned, "row-table-create-operation "+directory+".to_table"), qt.IsTrue, qt.Commentf("%v", planned))
	c.Assert(slices.Index(planned, "table-drop-operation "+directory+".to_node") <
		slices.Index(planned, "coordination-node-operation "+directory+".to_node"), qt.IsTrue, qt.Commentf("%v", planned))
	c.Assert(heldNode(c, opened, toNode).SelfCheckPeriodMillis, qt.Equals, uint32(2000))
	described, err := opened.Driver.Scheme().DescribePath(c.Context(), toTable)
	c.Assert(err, qt.IsNil)
	c.Assert(described.IsTable(), qt.IsTrue)
}

// Ported from the host's TestYDBCoordinationNodes_ReaderLeavesTheLockNodeOut:
// a read of the database root that holds Ptah's lock node and of a directory
// that holds a node of the same name leaves the first out, as the provider's
// own, and reads the second as an ordinary node.
func TestLockNodeIsLeftOutOfARootReadSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	c.Assert(opened.Driver.Coordination().CreateNode(ctx, path.Join(database, locking.NodeName), coordination.NodeConfig{}), qt.IsNil)
	c.Assert(opened.Driver.Coordination().CreateNode(ctx, path.Join(database, directory, locking.NodeName), coordination.NodeConfig{}), qt.IsNil)
	client := startProviderFor(c)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/", directory}})

	c.Assert(err, qt.IsNil)
	var nodes, reserved []string
	for _, object := range snapshot.NativeObjects {
		if object.Kind == schema.CoordinationProperties {
			nodes = append(nodes, object.Owner.Schema+"|"+object.Owner.Name)
		}
	}
	for _, object := range snapshot.ReservedObjects {
		reserved = append(reserved, object.NormalizedObject.Schema+"|"+object.NormalizedObject.Name)
	}
	c.Assert(nodes, qt.DeepEquals, []string{directory + "|" + locking.NodeName})
	c.Assert(reserved, qt.Contains, "|"+locking.NodeName)
}
