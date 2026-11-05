package backend_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// countsFeatures is the target profile of a host that lists the counts.
func countsFeatures() []string {
	return []string{provider.ComparisonCountsFeature}
}

// heldNode is a coordination node the read found with the given mode.
func heldNode(mode Ydb_Coordination.ConsistencyMode) *Ydb_Coordination.DescribeNodeResult {
	return &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{ReadConsistencyMode: mode}, Self: &Ydb_Scheme.Entry{Owner: "owner"}}
}

// countOf is the matched, added and removed of the only count of a reply.
func countOf(c *qt.C, result *protocol.ComparedModels, kind string) [3]uint32 {
	c.Helper()
	c.Assert(result.GetCounts(), qt.HasLen, 1)
	c.Assert(result.GetCounts()[0].GetKind(), qt.Equals, kind)
	return [3]uint32{result.GetCounts()[0].GetMatched(), result.GetCounts()[0].GetAdded(), result.GetCounts()[0].GetRemoved()}
}

// A coordination node both sides hold is matched whether or not it differs, a
// creation is added and a drop removed. The SDK checks each reply against its
// changes, so a count that disagrees with them fails here too.
func TestComparisonCountsDecidedObjects_HappyPath(t *testing.T) {
	tests := []struct {
		name     string
		desired  *schema.CoordinationIntent
		observed *Ydb_Coordination.DescribeNodeResult
		changes  int
		want     [3]uint32
	}{
		{name: "a node that agrees", desired: &schema.CoordinationIntent{}, observed: heldNode(Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED), want: [3]uint32{1, 0, 0}},
		{name: "a node that differs", desired: &schema.CoordinationIntent{}, observed: heldNode(Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT), changes: 1, want: [3]uint32{1, 0, 0}},
		{name: "a creation", desired: &schema.CoordinationIntent{}, changes: 1, want: [3]uint32{0, 1, 0}},
		{name: "a drop", observed: heldNode(Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT), changes: 1, want: [3]uint32{0, 0, 1}},
		{name: "nothing on either side", want: [3]uint32{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testcomparison.Request(test.desired, test.observed)
			c.Assert(err, qt.IsNil)
			request.TargetFeatures = countsFeatures()

			result, err := backend.New().CompareModels(t.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.GetChanges(), qt.HasLen, test.changes)
			c.Assert(countOf(c, result, schema.CoordinationChange), qt.Equals, test.want)
		})
	}
}

// An object the comparison excludes, keeps or leaves undecided counts on
// neither side, whichever side holds it.
func TestComparisonCountsLeaveOutWhatItDoesNotDecide_HappyPath(t *testing.T) {
	subject := &protocol.ComparisonIdentity{Kind: schema.CoordinationProperties, Database: testcomparison.Part("/local"), Schema: testcomparison.Part("app"), Name: testcomparison.Part("node")}
	exclusions := []*protocol.ComparisonExclusion{{Kind: schema.CoordinationProperties, Name: "app/node"}}
	complete, uninspected := protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_UNINSPECTED
	strict := heldNode(Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT)
	tests := []struct {
		name             string
		desired          *schema.CoordinationIntent
		observed         *Ydb_Coordination.DescribeNodeResult
		exclusions       []*protocol.ComparisonExclusion
		desiredKnowledge protocol.ModelKnowledge_State
		currentLimits    []*protocol.ComparisonSubjectCoverage
		wantUndecided    int
	}{
		{name: "an excluded node both sides hold", desired: &schema.CoordinationIntent{}, observed: strict, exclusions: exclusions, desiredKnowledge: complete},
		{name: "an excluded node only the source declares", desired: &schema.CoordinationIntent{}, exclusions: exclusions, desiredKnowledge: complete},
		{name: "an excluded node only the database holds", observed: strict, exclusions: exclusions, desiredKnowledge: complete},
		{name: "a node a source without a claim keeps", observed: strict, desiredKnowledge: uninspected},
		{name: "a node the read could not establish", desired: &schema.CoordinationIntent{}, observed: strict, desiredKnowledge: complete, wantUndecided: 1,
			currentLimits: []*protocol.ComparisonSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: subject, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNREPRESENTABLE)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testcomparison.Request(test.desired, test.observed)
			c.Assert(err, qt.IsNil)
			request.TargetFeatures = countsFeatures()
			request.Exclusions = test.exclusions
			request.Current.Coverage.Subjects = test.currentLimits
			request.Desired.Coverage.Kinds[0].Sources = []*protocol.ComparisonKnowledgeSource{{Outside: testcomparison.Knowledge(test.desiredKnowledge)}}

			result, err := backend.New().CompareModels(t.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.GetChanges(), qt.HasLen, 0)
			c.Assert(result.GetUndecided(), qt.HasLen, test.wantUndecided)
			c.Assert(countOf(c, result, schema.CoordinationChange), qt.Equals, [3]uint32{})
		})
	}
}

// A host that does not ask receives no counts, even of zeros, and the SDK
// refuses a reply that sends them: its kinds have no inventory entry.
func TestComparisonCountsNeedTheTargetFeature_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, heldNode(Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED))
	c.Assert(err, qt.IsNil)

	result, err := backend.New().CompareModels(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.GetCounts(), qt.HasLen, 0)
}

// emptyBatch asks a comparison service about kinds and holds nothing of them,
// as the host asks when only a coverage claim names a kind.
func emptyBatch(service *protocol.ModelComparison, kinds []string) *protocol.CompareModelsRequest {
	return &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: service.GetId(), Kinds: kinds, TargetFeatures: countsFeatures(), DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().GetEngines()[0].GetIdentifiers())}, Capabilities: make(map[string]bool),
		Desired: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}}
}

// comparisonService is the description's comparison service named id.
func comparisonService(c *qt.C, id string) *protocol.ModelComparison {
	c.Helper()
	services := backend.Description().GetEngines()[0].GetComparisons()
	index := slices.IndexFunc(services, func(service *protocol.ModelComparison) bool { return service.GetId() == id })
	c.Assert(index >= 0, qt.IsTrue, qt.Commentf("no comparison service %s", id))
	return services[index]
}

// objectServices names every object comparison the provider declares, which is
// every comparison that can count: a new one that forgets joins the failures
// of the test below.
func objectServices() []*protocol.ModelComparison {
	var services []*protocol.ModelComparison
	for _, service := range backend.Description().GetEngines()[0].GetComparisons() {
		if service.GetPlacement() == protocol.ModelComparison_OBJECTS {
			services = append(services, service)
		}
	}
	return services
}

// Every change kind of every object comparison has a count, or a drift
// inventory loses the kind. The SDK refuses a count of a kind the service does
// not declare, so this also checks the kind each family names.
func TestEveryObjectComparisonCountsEachChangeKind_HappyPath(t *testing.T) {
	services := objectServices()
	c := testcheck.New(t)
	c.Assert(len(services) >= 9, qt.IsTrue, qt.Commentf("read %d object comparisons", len(services)))
	for _, service := range services {
		t.Run(service.GetId(), func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(t.Context(), emptyBatch(service, service.GetKinds()))

			c.Assert(err, qt.IsNil)
			kinds := make([]string, len(result.GetCounts()))
			for i, count := range result.GetCounts() {
				kinds[i] = count.GetKind()
				c.Assert(count.GetMatched()+count.GetAdded()+count.GetRemoved(), qt.Equals, uint32(0))
			}
			c.Assert(kinds, qt.DeepEquals, service.GetChangeKinds())
		})
	}
}

// The external object comparison holds two change kinds, and the host names
// only the kinds a batch holds. A batch of one kind still counts the other,
// with zeros, or the batch's kind would be the only one the inventory has.
func TestExternalComparisonCountsTheKindABatchLeavesOut_HappyPath(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "a batch of data sources", kind: schema.ExternalSourceProperties},
		{name: "a batch of external tables", kind: schema.ExternalTableProperties},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			service := comparisonService(c, externalcompare.ServiceID)

			result, err := backend.New().CompareModels(t.Context(), emptyBatch(service, []string{test.kind}))

			c.Assert(err, qt.IsNil)
			c.Assert(result.GetCounts(), qt.HasLen, 2)
			c.Assert(result.GetCounts()[0].GetKind(), qt.Equals, schema.ExternalSourceChange)
			c.Assert(result.GetCounts()[1].GetKind(), qt.Equals, schema.ExternalTableChange)
		})
	}
}

// A table's settings stand for no object and its changefeeds change as one set,
// so the facet comparison counts nothing and its kinds have no inventory entry.
func TestRowTableComparisonCountsNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	service := comparisonService(c, tablecompare.ServiceID)

	request := emptyBatch(service, service.GetKinds())
	request.DatabasePath = ""

	result, err := backend.New().CompareModels(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.GetCounts(), qt.HasLen, 0)
}
