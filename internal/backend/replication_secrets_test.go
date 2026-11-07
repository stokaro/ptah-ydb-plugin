package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The tests here hold an async replication and a transfer to the secret their
// connection names by path. YDB looks the secret up when it creates the object
// or changes its connection (`secret ... not found`), so the secret is created
// first, and dropping it leaves the object naming a secret that is gone.

// remoteMirror is mirror reading another database, its token secret named by
// path, so it reads no source table here.
func remoteMirror(token string) replication.ReplicationSpec {
	spec := mirror()
	spec.Connection = replication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretPath: token}
	return spec
}

// TestReplicationPlanningActsOnWhatItNames_HappyPath names what a replication
// step acts on beyond its own path: a creation creates the replica at its
// target and reads the secret its connection names by path, by the secret's
// identity and by its path; a change of a paused replication's connection
// reads the secret too; a drop with CASCADE drops the replica; and the drop of
// a failed-over replication touches nothing beyond its own path. Last comes
// the directory a creation reads above its path, or the one a drop leaves room
// for at it. Ported from the host's TestReplicationPlan_EffectsAndPlacement.
func TestReplicationPlanningActsOnWhatItNames_HappyPath(t *testing.T) {
	moved := remoteMirror("secrets/token")
	moved.Connection.ConnectionString = "grpc://standby:2136/?database=/prod"
	for _, test := range []struct {
		name           string
		declared, read *replication.ReplicationSpec
		state          string
		want           []string
	}{
		{name: "a creation", declared: new(remoteMirror("secrets/token")), want: []string{
			schema.SecretProperties + " |secrets||token READ", "ydb.scheme-path |secrets||token READ", "ydb.scheme-path |replica||accounts CREATE",
			"ydb.scheme-directory |||app READ"}},
		{name: "a paused replication moved", declared: new(remoteMirror("secrets/token")), read: &moved, state: replication.StatePaused, want: []string{
			schema.SecretProperties + " |secrets||token READ", "ydb.scheme-path |secrets||token READ"}},
		{name: "a running replication dropped", read: new(remoteMirror("secrets/token")), state: replication.StateRunning, want: []string{
			"ydb.scheme-path |replica||accounts DROP", "ydb.scheme-directory |app||mirror CREATE"}},
		{name: "a failed-over replication dropped", read: new(remoteMirror("secrets/token")), state: replication.StateDone, want: []string{
			"ydb.scheme-directory |app||mirror CREATE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)

			planned := plan(c, replicationplan.ReplicationServiceID, result.Changes[0])

			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(reads(planned), qt.DeepEquals, test.want)
		})
	}
}

// A replication's creation reads the secret by the identity the secret
// planner gives the step that creates it, so the host orders the secret's
// creation first when the same plan creates both. Ported from the host's
// TestPlan_OwnedReplication_CreatedAfterTheTableAtItsTarget.
func TestReplicationReadsTheSecretThePlanCreates_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, new(remoteMirror("app/pg.pw"))), nil)
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	secrets := secretRequest(c, secretAbsent)
	secretChanges, err := backend.New().CompareModels(t.Context(), secrets)
	c.Assert(err, qt.IsNil)
	c.Assert(secretChanges.Changes, qt.HasLen, 1)
	secretPlan, err := backend.New().PlanFeatures(t.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: secretplan.ServiceID, TargetFeatures: []string{provider.PlanningLifecycleFeature},
		Identifiers: secrets.Identifiers, Capabilities: secrets.Capabilities, Changes: []*protocol.PlanningChange{{Subject: secretChanges.Changes[0].Subject, Value: secretChanges.Changes[0].Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(secretPlan.Diagnostics, qt.HasLen, 0)

	planned := plan(c, replicationplan.ReplicationServiceID, result.Changes[0])

	c.Assert(planned.Diagnostics, qt.HasLen, 0)
	c.Assert(sharedSubjects(secretPlan.Contributions[0].Steps[0], protocol.PlanningEffect_CREATE, planned.Contributions[0].Steps[0], protocol.PlanningEffect_READ),
		qt.DeepEquals, []string{schema.SecretProperties + " app/pg.pw", "ydb.scheme-path app/pg.pw"})
}

// sharedSubjects names each subject first acts on with firstAction that second
// acts on with secondAction, as its kind and path, in first's order.
func sharedSubjects(first *protocol.PlanningStep, firstAction protocol.PlanningEffect_Action, second *protocol.PlanningStep, secondAction protocol.PlanningEffect_Action) []string {
	var shared []string
	for _, own := range first.GetMetadata().GetEffects() {
		for _, other := range second.GetMetadata().GetEffects() {
			if own.GetAction() == firstAction && other.GetAction() == secondAction && provider.CompareIdentities(own.GetSubject(), other.GetSubject()) == 0 {
				shared = append(shared, own.GetSubject().GetKind()+" "+own.GetSubject().GetSchema().GetNormalized()+"/"+own.GetSubject().GetName().GetNormalized())
			}
		}
	}
	return shared
}

// A declared replication or transfer requires the secret its connection names
// by path, so a source that leaves the secret out asks for a drop the secret
// comparison refuses by name, as the host's engine refuses a plan that drops a
// secret one of its statements reads. Ported from the host's
// TestPlan_OwnedReplication_RefusesADroppedSecretItReads.
func TestSecretComparisonRefusesDroppingASecretAReplicationReads_FailurePath(t *testing.T) {
	transfer := ingest()
	transfer.Connection = replication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", PasswordSecretPath: "app/pg.pw", User: "reader"} // #nosec G101 -- a secret's path, not its value
	for _, test := range []struct {
		name        string
		replication *replication.ReplicationSpec
		transfer    *replication.TransferSpec
	}{
		{name: "an async replication's token", replication: new(remoteMirror("app/pg.pw"))},
		{name: "a transfer's password", transfer: &transfer},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			required := requiredByDeclaration(c, declaredReplication(c, test.replication), declaredTransfer(c, test.transfer))
			request := secretRequest(c, secretPresent)
			request.Required = required
			request.Desired.Records = nil

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = secret app/pg.pw is dropped while a declared object reads it by its path`)
			c.Assert(result, qt.IsNil)
		})
	}
}

// requiredByDeclaration is what the declarations among values require, each
// normalized as the host normalizes a declaration; a nil value is none.
func requiredByDeclaration(c *qt.C, values ...*protocol.Envelope) []*protocol.ComparisonIdentity {
	c.Helper()
	var required []*protocol.ComparisonIdentity
	for _, value := range values {
		if value == nil {
			continue
		}
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "v", Representation: protocol.ValueInput_DESIRED, Value: value}}})
		c.Assert(err, qt.IsNil)
		required = append(required, normalized.Values[0].RequiredObjects...)
	}
	return required
}

// A replication that names its secret by name, or no secret at all, requires
// nothing, and the secret's drop is planned.
func TestSecretComparisonDropsASecretNoReplicationReads_HappyPath(t *testing.T) {
	byName := mirror()
	c := testcheck.New(t)
	required := requiredByDeclaration(c, declaredReplication(c, &byName))
	request := secretRequest(c, secretPresent)
	request.Required = required
	request.Desired.Records = nil

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(required, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 1)
}

// YDB reports the secret a replication names by path as an absolute path, and
// a read replication requires the secret that path names in its database. A
// path outside the database names no secret this database holds and requires
// nothing.
func TestReadReplicationRequiresTheSecretItsAbsolutePathNames_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, path string
		want       []string
	}{
		{name: "a path under the database", path: "/local/secrets/token", want: []string{schema.SecretProperties + " /local|secrets|token"}},
		{name: "a path outside the database", path: "/other/secrets/token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := readReplication(c, new(remoteMirror(test.path)), replication.StateRunning)

			normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "v", Representation: protocol.ValueInput_OBSERVED, Value: read}}})

			c.Assert(err, qt.IsNil)
			var required []string
			for _, identity := range normalized.Values[0].RequiredObjects {
				required = append(required, identity.GetKind()+" "+identity.GetDatabase().GetNormalized()+"|"+identity.GetSchema().GetNormalized()+"|"+identity.GetName().GetNormalized())
			}
			c.Assert(required, qt.DeepEquals, test.want)
		})
	}
}

// transferOf is ingest reading source through connection, the zero
// connection for a topic of its own database.
func transferOf(source string, connection replication.Connection) *replication.TransferSpec {
	transfer := ingest()
	transfer.Source, transfer.Connection = source, connection
	return &transfer
}

// A declared transfer of a topic of its own database requires the topic, so a
// source that keeps the transfer and leaves the topic out asks for a drop the
// topic comparison refuses by name: YDB drops the topic and keeps the
// transfer, which then fails (`Discovery for all topics failed`), measured on
// 26.2.1.14. The host's embedded engine refused the plan. Ported from the
// host's TestGenerateMigrationAST_Replications_TransferReadsADeclaredTopic.
func TestTopicComparisonRefusesDroppingATopicATransferReads_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := topicRequest(c, nil, readTopic())
	request.Required = requiredByDeclaration(c, declaredTransfer(c, transferOf("app/events", replication.Connection{})))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = topic app/events is dropped while a declared transfer reads it, "+
		"and YDB keeps the transfer, which then stops \\(`Discovery for all topics failed`\\); declare the topic, or remove the transfer")
	c.Assert(result, qt.IsNil)
}

// YDB reports the topic a transfer of its own database reads by its absolute
// path, and a read transfer requires the topic that path names in its
// database. A path outside the database names no topic this database
// holds and requires none.
func TestReadTransferRequiresTheTopicItsAbsolutePathNames_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         []string
	}{
		{name: "a path under the database", source: "/local/app/events", want: []string{schema.TopicProperties + " /local|app|events"}},
		{name: "a path outside the database", source: "/other/app/events"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := readTransfer(c, transferOf(test.source, replication.Connection{}), replication.StateRunning)

			normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "v", Representation: protocol.ValueInput_OBSERVED, Value: read}}})

			c.Assert(err, qt.IsNil)
			var required []string
			for _, identity := range normalized.Values[0].RequiredObjects {
				required = append(required, identity.GetKind()+" "+identity.GetDatabase().GetNormalized()+"|"+identity.GetSchema().GetNormalized()+"|"+identity.GetName().GetNormalized())
			}
			c.Assert(required, qt.DeepEquals, test.want)
		})
	}
}

// A transfer of another topic, or of another database's topic, requires none
// of this database's topics, and the topic's drop is planned.
func TestTopicComparisonDropsATopicNoTransferReads_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		transfer *replication.TransferSpec
	}{
		{name: "another topic", transfer: transferOf("app/other", replication.Connection{})},
		{name: "another database's topic", transfer: transferOf("app/events", replication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicRequest(c, nil, readTopic())
			request.Required = requiredByDeclaration(c, declaredTransfer(c, test.transfer))

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
		})
	}
}
