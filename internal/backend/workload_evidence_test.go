package backend_test

import (
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// workloadKnowledge adds what state knows of the object of kind named name.
func workloadKnowledge(state *protocol.ComparisonState, kind, name, database string, knowledge protocol.ModelKnowledge_State) {
	state.Coverage.Subjects = append(state.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: kind, Subject: workloadSubject(kind, name, database),
		Knowledge: testcomparison.Knowledge(knowledge)})
}

// Ported from the host's TestPoolComparisonPreservesLimitsAndEvidence: a zero
// limit both sides hold is no change, and a pool read in a namespace the read
// did not enumerate is still altered, since the read holds it; a pool either
// side cannot describe completely is not altered, and the comparison says so.
func TestPoolComparisonReadsLimitsAndEvidence(t *testing.T) {
	zero, unlimited := pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0))}), pool(workload.PoolSpec{})
	for _, test := range []struct {
		name                       string
		read                       proto.Message
		currentKnowledge           protocol.ModelKnowledge_State
		desiredLimit, currentLimit protocol.ModelKnowledge_State
		changes, undecided         int
	}{
		{name: "a matching zero", read: zero},
		{name: "an unknown current pool withholds the alteration", read: unlimited, currentLimit: protocol.ModelKnowledge_UNREPRESENTABLE, undecided: 1},
		{name: "a known pool in an incomplete namespace", read: unlimited, currentKnowledge: protocol.ModelKnowledge_UNINSPECTED, changes: 1},
		{name: "an incomplete declaration withholds the alteration", read: unlimited, desiredLimit: protocol.ModelKnowledge_UNREPRESENTABLE, undecided: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"batch": zero}, map[string]proto.Message{"batch": test.read})
			request.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(orComplete(test.currentKnowledge))
			for _, limit := range stated(test.desiredLimit) {
				workloadKnowledge(request.Desired, schema.ResourcePoolProperties, "batch", "", limit)
			}
			for _, limit := range stated(test.currentLimit) {
				workloadKnowledge(request.Current, schema.ResourcePoolProperties, "batch", "/local", limit)
			}

			result := compareWorkload(c, request)

			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
		})
	}
}

// Ported from the host's
// TestWorkloadComparisonRefusesInvalidRequestsWithoutPartialOutput: a pool and
// a classifier are objects of the database, never of a directory, and the
// default pool takes no concurrent query limit, so such a declaration is
// refused when it is captured, before anything is compared.
func TestWorkloadDeclarationRefusesWhatYDBCannotHold(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     string
		dir      string
		object   string
		settings proto.Message
		want     string
	}{
		{name: "a limit on the default pool", kind: schema.ResourcePoolProperties, object: "default", settings: pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0))}),
			want: `resource pool "default": the pool default takes no concurrent_query_limit: YDB keeps it unlimited .*`},
		{name: "a pool in a directory", kind: schema.ResourcePoolProperties, dir: "app", object: "batch", settings: pool(workload.PoolSpec{}),
			want: `workload objects require exact database-scoped YDB identities`},
		{name: "a classifier in a directory", kind: schema.ResourcePoolClassifierProperties, dir: "app", object: "etl", settings: routing("default", 1),
			want: `workload objects require exact database-scoped YDB identities`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: test.kind, Schema: test.dir, Name: test.object}, test.settings)

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(value, qt.IsNil)
		})
	}
}

// Ported from the host's TestWorkloadComparisonRetainsUnmanagedObjectsWithoutDDLSupport:
// seeing a database's workload object is no request to change it. A source
// that leaves one out keeps it as YDB holds it, whether or not the read
// enumerated the whole kind and whether or not the target can change
// resource pools, and adopts it as a declaration.
func TestWorkloadComparisonKeepsAnObjectTheReadHolds(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         string
		read         map[string]proto.Message
		knowledge    protocol.ModelKnowledge_State
		capabilities map[string]bool
	}{
		{name: "a classifier on a target without resource pools", kind: schema.ResourcePoolClassifierProperties, read: map[string]proto.Message{"etl": routing("default", 0)},
			capabilities: map[string]bool{workload.CapabilityResourcePools: false}},
		{name: "a pool of a complete read", kind: schema.ResourcePoolProperties, read: map[string]proto.Message{"batch": pool(workload.PoolSpec{})}},
		{name: "a pool of a partial read", kind: schema.ResourcePoolProperties, read: map[string]proto.Message{"batch": pool(workload.PoolSpec{})}, knowledge: protocol.ModelKnowledge_UNINSPECTED},
		{name: "a classifier of a complete read", kind: schema.ResourcePoolClassifierProperties, read: map[string]proto.Message{"etl": routing("default", 0)}},
		{name: "a classifier of a partial read", kind: schema.ResourcePoolClassifierProperties, read: map[string]proto.Message{"etl": routing("default", 0)},
			knowledge: protocol.ModelKnowledge_UNINSPECTED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := workloadRequest(c, test.kind, nil, test.read)
			request.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(orComplete(test.knowledge))
			maps.Copy(request.Capabilities, test.capabilities)

			result := compareWorkload(c, request)

			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Desired.Records, qt.HasLen, 1)
			c.Assert(result.Desired.Records[0].Value.Value.Kind, qt.Equals, test.kind)
		})
	}
}

// Ported from the host's TestWorkloadComparisonKeepsUnmanagedSubjectLimits: a
// read pool the read cannot describe completely, under a source that makes no
// claim about pools, is neither changed nor adopted, and the comparison says
// so.
func TestWorkloadComparisonKeepsAnUnreadablePoolUndecided(t *testing.T) {
	c := testcheck.New(t)
	request := workloadRequest(c, schema.ResourcePoolProperties, nil, map[string]proto.Message{"batch": pool(workload.PoolSpec{})})
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	request.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	workloadKnowledge(request.Current, schema.ResourcePoolProperties, "batch", "/local", protocol.ModelKnowledge_UNREPRESENTABLE)

	result := compareWorkload(c, request)

	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Desired.Records, qt.HasLen, 0)
}
