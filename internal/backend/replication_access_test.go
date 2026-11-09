package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replicationAccess is one replication or transfer change a plan makes beside
// a host step that grants, or revokes, a permission on the object.
type replicationAccess struct {
	name                       string
	kind, comparison, planning string
	object, grantKind          string
	declared, read             *protocol.Envelope
	verb                       accessVerb
}

// replicationAccessCases are the changes of both families, with the
// operations each change can carry.
func replicationAccessCases(c *qt.C) (creations, drops []replicationAccess) {
	c.Helper()
	paused := mirror()
	paused.Connection = replication.Connection{ConnectionString: paused.Connection.ConnectionString, User: "replicator", PasswordSecretName: "password"}
	relambda := ingest()
	relambda.Lambda = "($m) -> { return []; }"
	replications := replicationAccess{kind: schema.ReplicationProperties, comparison: replicationcompare.ReplicationServiceID,
		planning: replicationplan.ReplicationServiceID, object: "mirror", grantKind: "async replication"}
	transfers := replicationAccess{kind: schema.TransferProperties, comparison: replicationcompare.TransferServiceID,
		planning: replicationplan.TransferServiceID, object: "ingest", grantKind: "transfer"}
	with := func(base replicationAccess, name string, declared, read *protocol.Envelope, verb accessVerb) replicationAccess {
		base.name, base.declared, base.read, base.verb = name, declared, read, verb
		return base
	}
	creations = []replicationAccess{
		with(replications, "a replication created", declaredReplication(c, new(mirror())), nil, grantAccess),
		with(replications, "a paused replication altered", declaredReplication(c, &paused), readReplication(c, new(mirror()), replication.StatePaused), grantAccess),
		with(replications, "a paused replication altered with a revoke", declaredReplication(c, &paused), readReplication(c, new(mirror()), replication.StatePaused), revokeAccess),
		with(transfers, "a transfer created", declaredTransfer(c, new(ingest())), nil, grantAccess),
		with(transfers, "a transfer's lambda altered", declaredTransfer(c, &relambda), readTransfer(c, new(ingest()), replication.StateRunning), grantAccess),
	}
	drops = []replicationAccess{
		with(replications, "a running replication dropped", nil, readReplication(c, new(mirror()), replication.StateRunning), revokeAccess),
		with(transfers, "a transfer dropped", nil, readTransfer(c, new(ingest()), replication.StateRunning), revokeAccess),
	}
	return creations, drops
}

// planAccess compares the object of test and plans its one change beside the
// host's access step, with the footprint the host gives a grant on an object
// a scope selection names: a read of the object and an alteration of its
// access list.
func planAccess(c *qt.C, test replicationAccess) (*protocol.PlannedFeatures, *protocol.CommonPlanningStep) {
	c.Helper()
	compared, err := backend.New().CompareModels(c.Context(), objectRequest(c, test.kind, test.comparison, test.object, test.declared, test.read))
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	access := commonAccessFor(compared.Changes[0].Subject, test.grantKind, test.verb)
	planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: test.planning, DatabasePath: "/local",
		TargetFeatures: []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature},
		Identifiers:    &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: replicationCapabilities(),
		Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}, CommonSteps: []*protocol.CommonPlanningStep{access}})
	c.Assert(err, qt.IsNil)
	return planned, access
}

// A replication or a transfer the plan creates or changes in place comes
// before a grant or a revoke on it: YDB grants nothing on a path that holds
// no object, and the access the plan read is checked before it changes.
func TestReplicationAccessFollowsTheObject_HappyPath(t *testing.T) {
	creations, _ := replicationAccessCases(testcheck.New(t))
	for _, test := range creations {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			planned, access := planAccess(c, test)
			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(planned.Rewrites, qt.HasLen, 0)
			c.Assert(planned.Contributions, qt.HasLen, 1)
			step := planned.Contributions[0].Steps[0]
			c.Assert(planned.Contributions[0].Dependencies, qt.HasLen, 1)
			c.Assert(proto.Equal(planned.Contributions[0].Dependencies[0], &protocol.PlanningDependency{Before: step.Id, After: access.Id}), qt.IsTrue)
			c.Assert(step.Metadata.Effects, qt.Any(qt.CmpEquals(protocmp.Transform())), &protocol.PlanningEffect{Subject: access.Metadata.Effects[1].Subject, Action: protocol.PlanningEffect_READ})
		})
	}
}

// A drop takes the object's access list with it, so the drop step replaces
// the revoke the host planned, which would otherwise name a path that is gone.
func TestReplicationAccessDropTakesTheRevoke_HappyPath(t *testing.T) {
	_, drops := replicationAccessCases(testcheck.New(t))
	for _, test := range drops {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			planned, access := planAccess(c, test)
			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(planned.Contributions, qt.HasLen, 1)
			step := planned.Contributions[0].Steps[0]
			c.Assert(planned.Rewrites, qt.HasLen, 1)
			c.Assert(proto.Equal(planned.Rewrites[0], &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{access.Id}, Replacement: step.Id}), qt.IsTrue)
			c.Assert(step.Metadata.Effects, qt.Any(qt.CmpEquals(protocmp.Transform())), &protocol.PlanningEffect{Subject: access.Metadata.Effects[1].Subject, Action: protocol.PlanningEffect_ALTER})
		})
	}
}

// A grant on an object the plan drops, and a revoke on one the plan creates,
// contradict the plan and are refused.
func TestReplicationAccessContradictingTheObject_FailurePath(t *testing.T) {
	creations, drops := replicationAccessCases(testcheck.New(t))
	granted := drops[0]
	granted.name, granted.verb = "a grant on a dropped replication", grantAccess
	revoked := creations[0]
	revoked.name, revoked.verb = "a revoke on a created replication", revokeAccess
	transfer := drops[1]
	transfer.name, transfer.verb = "a grant on a dropped transfer", grantAccess
	for _, test := range []replicationAccess{granted, revoked, transfer} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			planned, _ := planAccess(c, test)
			c.Assert(planned.Diagnostics, qt.HasLen, 1)
			c.Assert(planned.Contributions, qt.HasLen, 0)
			c.Assert(planned.Rewrites, qt.HasLen, 0)
		})
	}
}
