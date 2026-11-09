package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replicationTo replicates source into target in this database.
func replicationTo(source, target string) replication.ReplicationSpec {
	return replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local", TokenSecretName: "token"},
		Items: []replication.Item{{Source: source, Target: target}}}
}

// declaredAt declares spec at app/name.
func declaredAt(c *qt.C, name string, spec replication.ReplicationSpec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureReplicationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Schema: "app", Name: name}, spec)
	c.Assert(err, qt.IsNil)
	return value
}

// readAt reads spec, running, at /local/app/name.
func readAt(c *qt.C, name string, spec replication.ReplicationSpec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Database: "/local", Schema: "app", Name: name},
		replication.ObservedReplication(spec, replication.StateRunning))
	c.Assert(err, qt.IsNil)
	return value
}

// replicationsIn is the context state of representation that holds values.
func replicationsIn(c *qt.C, representation protocol.ValueInput_Representation, values ...*protocol.Envelope) *protocol.ComparisonState {
	c.Helper()
	state := replicationContextState(c, representation, nil)
	for _, value := range values {
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Features,
			Values: []*protocol.ValueInput{{Id: value.Owner.Name, Representation: representation, Value: value}}})
		c.Assert(err, qt.IsNil)
		state.Records = append(state.Records, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}
	return state
}

// changeOf is the change the comparison of app/name finds between declared
// and read, either of which may be nil, under a value ID of its own.
func changeOf(c *qt.C, name string, declared, read *protocol.Envelope) *protocol.PlanningChange {
	c.Helper()
	compared, err := backend.New().CompareModels(c.Context(), objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, name, declared, read))
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	value := proto.CloneOf(compared.Changes[0].Value)
	value.Id = "change/" + name
	return &protocol.PlanningChange{Subject: compared.Changes[0].Subject, Value: value}
}

// overlapRequest plans changes beside the replications the database holds
// and the ones the schema declares.
func overlapRequest(c *qt.C, held, declared []*protocol.Envelope, changes ...*protocol.PlanningChange) *protocol.PlanFeaturesRequest {
	c.Helper()
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local", ContextKinds: []string{schema.ReplicationProperties},
		TargetFeatures: backend.Description().Features, Capabilities: replicationCapabilities(),
		Identifiers:    &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		CurrentObjects: replicationsIn(c, protocol.ValueInput_OBSERVED, held...), DesiredObjects: replicationsIn(c, protocol.ValueInput_DESIRED, declared...), Changes: changes}
}

// A replication the plan creates at, above or below the target of another
// replication the plan keeps or creates is refused: YDB creates it over that
// replication's replica, both write the replica, and a drop of either with
// CASCADE drops it while the other runs on without it (measured on 25.1.4.7
// and 26.2.1.14). The host's embedded engine at 74de0b65b refused such a plan
// (TestGenerateMigrationAST_Replications_FailurePath, "a replication created
// over another's replica"); this provider planned and applied it (G242).
func TestReplicationPlanningRefusesAReplicaOfAnother_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		held                bool
		want                string
	}{
		{name: "at another's replica", first: "replica/accounts", second: "replica/accounts", held: true,
			want: "its target replica/accounts overlaps replica/accounts, where async replication app/first writes its replica; "},
		{name: "above another's replica", first: "replica/accounts", second: "replica", held: true,
			want: "its target replica overlaps replica/accounts, where async replication app/first writes its replica; "},
		{name: "below another's replica directory", first: "replica", second: "replica/accounts", held: true,
			want: "its target replica/accounts overlaps replica, where async replication app/first writes its replica; "},
		{name: "beside another the plan creates", first: "replica/accounts", second: "replica/accounts",
			want: "its target replica/accounts overlaps replica/accounts, where async replication app/first writes its replica; "},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			first, second := replicationTo("src/accounts", test.first), replicationTo("src2/accounts", test.second)
			held := map[bool][]*protocol.Envelope{true: {readAt(c, "first", first)}, false: nil}[test.held]
			request := overlapRequest(c, held, []*protocol.Envelope{declaredAt(c, "first", first), declaredAt(c, "second", second)},
				changeOf(c, "second", declaredAt(c, "second", second), nil))

			planned, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(planned.Contributions, qt.HasLen, 0)
			c.Assert(planned.Diagnostics, qt.HasLen, 1)
			c.Assert(planned.Diagnostics[0].Message, qt.Equals, "async replication app/second: "+test.want+
				"YDB creates the replication over that replica, both then write it, and a drop of either with CASCADE drops it "+
				"while the other runs on without it (measured on 25.1.4.7 and 26.2.1.14)")
		})
	}
}

// A replication beside another whose targets do not meet is planned, and so
// is one at the target of a replication the same plan drops, which goes
// first with its replica.
func TestReplicationPlanningCreatesBesideAnother_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	first := replicationTo("src/accounts", "replica/accounts")
	beside := replicationTo("src2/accounts", "replica/orders")
	again := replicationTo("src2/accounts", "replica/accounts")

	apart, apartErr := backend.New().PlanFeatures(c.Context(), overlapRequest(c, []*protocol.Envelope{readAt(c, "first", first)},
		[]*protocol.Envelope{declaredAt(c, "first", first), declaredAt(c, "second", beside)}, changeOf(c, "second", declaredAt(c, "second", beside), nil)))
	replaced, replacedErr := backend.New().PlanFeatures(c.Context(), overlapRequest(c, []*protocol.Envelope{readAt(c, "first", first)},
		[]*protocol.Envelope{declaredAt(c, "second", again)}, changeOf(c, "first", nil, readAt(c, "first", first)), changeOf(c, "second", declaredAt(c, "second", again), nil)))

	c.Assert(apartErr, qt.IsNil)
	c.Assert(apart.Diagnostics, qt.HasLen, 0)
	c.Assert(apart.Contributions, qt.HasLen, 1)
	c.Assert(replacedErr, qt.IsNil)
	c.Assert(replaced.Diagnostics, qt.HasLen, 0)
	c.Assert(replaced.Contributions, qt.HasLen, 1)
	c.Assert(replaced.Contributions[0].Steps, qt.HasLen, 2)
}
