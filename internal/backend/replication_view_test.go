package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// viewStep is a common step that creates the view replica/v over body, named
// as the host names it: its only effect is the view's creation.
func viewStep(body string) *protocol.CommonPlanningStep {
	view := &protocol.ComparisonIdentity{Kind: "view", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: "replica", Normalized: "replica"}, Parent: &protocol.ComparisonPart{}, Name: &protocol.ComparisonPart{Source: "v", Normalized: "v"}}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "view"},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a view",
			Effects: []*protocol.PlanningEffect{{Subject: view, Action: protocol.PlanningEffect_CREATE}}},
		CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
			View: &protocol.SQLView{Schema: "replica", Name: "v", Body: body}}}}}
}

// planBesideCommonObjects is planWith for a host that carries common objects,
// so a common step can name the view it creates.
func planBesideCommonObjects(c *qt.C, change *protocol.ComparedChange, common []*protocol.CommonPlanningStep) *protocol.PlannedFeatures {
	c.Helper()
	planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb",
		TargetFeatures: []string{provider.CommonObjectsFeature, provider.PlanningLifecycleFeature}, PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: replicationCapabilities(),
		Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}, CommonSteps: common})
	c.Assert(err, qt.IsNil)
	return planned
}

// TestReplicationPlanningPrecedesAViewOverItsReplica orders a replication's
// creation before a common step that creates a view whose query reads the
// replica table the replication creates, by a relative or an absolute path:
// YDB refuses a view over a path nothing holds yet. A view over another table
// comes before the creation, which follows the last common step that needs
// none of the replication's steps. The YDB engine built into the host at
// 74de0b65b created the replication first (TestGenerateMigrationAST_Replications_HappyPath).
func TestReplicationPlanningPrecedesAViewOverItsReplica(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       []string
	}{
		{name: "a view over the replica", body: "SELECT id FROM `replica/accounts`", want: []string{"step -> view"}},
		{name: "a view over the replica by its absolute path", body: "SELECT id FROM `/local/replica/accounts`", want: []string{"step -> view"}},
		{name: "a view over another table", body: "SELECT id FROM `src/accounts`", want: []string{"view -> step"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, new(mirror())), readReplication(c, nil, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)

			planned := planBesideCommonObjects(c, result.Changes[0], []*protocol.CommonPlanningStep{viewStep(test.body)})

			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(edges(planned), qt.DeepEquals, test.want)
		})
	}
}

// TestReplicationPlanningAwaitsTheReplicaAViewReads lowers a replication's
// creation to the operation that waits for its replica tables where a common
// step of the plan reads one, a view over it here, and to the one that goes on
// at once where none does: YDB creates the replica tables after CREATE ASYNC
// REPLICATION returns, and refuses a view over a table not there yet (G231).
func TestReplicationPlanningAwaitsTheReplicaAViewReads(t *testing.T) {
	for _, test := range []struct {
		name   string
		common []*protocol.CommonPlanningStep
		want   string
	}{
		{name: "a view over the replica", common: []*protocol.CommonPlanningStep{viewStep("SELECT id FROM `replica/accounts`")}, want: schema.ReplicationAwaitReplicas},
		{name: "a view over another table", common: []*protocol.CommonPlanningStep{viewStep("SELECT id FROM `src/accounts`")}, want: schema.ReplicationRun},
		{name: "no common step", want: schema.ReplicationRun},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, new(mirror())), readReplication(c, nil, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)

			planned := planBesideCommonObjects(c, result.Changes[0], test.common)

			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(planned.Contributions, qt.HasLen, 1)
			operation, err := schema.ReplicationOperationName(planned.Contributions[0].Steps[0].Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(operation, qt.Equals, test.want)
		})
	}
}
