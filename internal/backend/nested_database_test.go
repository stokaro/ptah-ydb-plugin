package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// claimNestedDatabase adds to state the claim a read makes for the root of
// the nested database at root, and with subtree for the directories below it:
// complete, since the read's database holds nothing there, with the nested
// database's reason.
func claimNestedDatabase(state *protocol.ComparisonState, root string, subtree bool) {
	source := state.Coverage.Kinds[0].Sources[0]
	source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Schema: testcomparison.Part(root), SchemaSubtree: subtree},
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.NestedDatabaseReason(root)}})
}

// A coordination node declared beside a nested database, in a directory whose
// name only starts with the root's or in a directory above the root, is a
// creation as on a server that serves none.
func TestComparisonRefusesAnObjectInANestedDatabase_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, root string
		subtree    bool
	}{
		{name: "a root whose name the directory starts with", root: "ap", subtree: true},
		{name: "a root below the node's directory", root: "app/node", subtree: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
			c.Assert(err, qt.IsNil)
			claimNestedDatabase(request.Current, test.root, test.subtree)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Undecided, qt.HasLen, 0)
		})
	}
}

// A coordination node declared where the read claims a nested database is
// refused in words that name it. The read holds no node there, so without the
// refusal the comparison reports a creation that YDB runs into the nested
// database.
func TestComparisonRefusesAnObjectInANestedDatabase_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, root string
		subtree    bool
	}{
		{name: "the claim of a selected directory", root: "app"},
		{name: "the claim of a whole database's read", root: "app", subtree: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
			c.Assert(err, qt.IsNil)
			claimNestedDatabase(request.Current, test.root, test.subtree)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.ErrorMatches, `.*this plan creates the coordination node app/node inside /local/app, `+
				`which is another YDB database nested in /local: a session of /local neither reads nor drops what it holds\. `+
				`Declare the coordination node in a source for /local/app and apply that source there`)
			c.Assert(result, qt.IsNil)
		})
	}
}

// The streaming query service's call refuses a plan that creates a native
// object inside a nested database, which no other service would see beside
// the claim of every kind's read.
func TestPlanFeaturesRefusesACreationInANestedDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := directoryConflictRequest(c, "app", "db2")
	claimNestedDatabase(request.CurrentObjects, "db2", true)

	result, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, `this plan creates the object db2/holder inside /local/db2, which is another YDB database nested in /local: .*`)
	c.Assert(result.Diagnostics[0].GetCode(), qt.Equals, protocol.PlanningDiagnostic_INVALID_SCHEMA)
	c.Assert(result.Contributions, qt.HasLen, 0)
}
