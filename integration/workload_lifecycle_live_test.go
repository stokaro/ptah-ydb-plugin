//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// heldPool is a pool as .sys/resource_pools reports it, -1 for no limit.
type heldPool struct {
	ConcurrentQueryLimit, QueueSize int32
	ResourceWeight                  float64
}

// heldClassifier is a classifier as .sys/resource_pool_classifiers reports
// it.
type heldClassifier struct {
	Pool, Member string
	Rank         int64
}

// readHeldWorkload reads the named pools and classifiers back from the
// system views; one the database does not hold is left out.
func readHeldWorkload(c *qt.C, ctx context.Context, db *sql.DB, database string, pools, classifiers []string) (map[string]heldPool, map[string]heldClassifier) {
	c.Helper()
	heldPools, heldClassifiers := map[string]heldPool{}, map[string]heldClassifier{}
	for _, name := range pools {
		var held heldPool
		err := db.QueryRowContext(ctx, "SELECT ConcurrentQueryLimit, QueueSize, ResourceWeight FROM `"+path.Join(database, ".sys/resource_pools")+"` WHERE Name = $name",
			sql.Named("name", name)).Scan(&held.ConcurrentQueryLimit, &held.QueueSize, &held.ResourceWeight)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		c.Assert(err, qt.IsNil)
		heldPools[name] = held
	}
	for _, name := range classifiers {
		var held heldClassifier
		err := db.QueryRowContext(ctx, "SELECT ResourcePool, MemberName, Rank FROM `"+path.Join(database, ".sys/resource_pool_classifiers")+"` WHERE Name = $name",
			sql.Named("name", name)).Scan(&held.Pool, &held.Member, &held.Rank)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		c.Assert(err, qt.IsNil)
		heldClassifiers[name] = held
	}
	return heldPools, heldClassifiers
}

// enableResourcePools turns the cluster's EnableResourcePools flag on for the
// test, through the dynamic configuration: YDB keeps it off by default on
// every line, and a session reads it from the monitoring endpoint.
func enableResourcePools(c *qt.C, opened *connection.Connection) {
	c.Helper()
	monitoredAddress(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableResourcePools": true})
}

// workloadRanks are two ranks of the database no other run of this test
// takes, since a rank is the database's, not the directory's.
func workloadRanks(directory string) (int64, int64) {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(directory))
	base := 10000 + int64(sum.Sum32()%100000)*2
	return base, base + 1
}

// TestWorkloadLifecycleSubprocess drives resource pools and classifiers
// through the provider's services the way the host does, and reads them back
// from the system views after each step: a creation, a rank swap written as
// a migration file's text, a change another writer made after the plan read
// the pool, and a removal.
func TestWorkloadLifecycleSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	poolName, first, second := directory+"_pool", directory+"_a", directory+"_b"
	low, high := workloadRanks(directory)
	pools, classifiers := []string{poolName}, []string{first, second}
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, name := range classifiers {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL CLASSIFIER `"+name+"`")
		}
		_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL `"+poolName+"`")
	})
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities[workload.CapabilityResourcePools], qt.IsTrue, qt.Commentf("the live target needs EnableResourcePools and its monitoring endpoint"))
	rules := proto.CloneOf(client.Description().Engines[0].Identifiers)

	// plan compares the declared pools and classifiers with what the
	// database holds and plans the changes that follow in one request.
	plan := func(form protocol.RenderRequest_Form, declaredPools map[string]workload.PoolSpec, declaredClassifiers map[string]workload.ClassifierSpec) []*protocol.Unit {
		c.Helper()
		snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/"}})
		c.Assert(err, qt.IsNil)
		planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: workloadplan.ServiceID, Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}}
		for _, family := range []struct {
			kind, service string
			declared      map[string]proto.Message
		}{
			{schema.ResourcePoolProperties, workloadcompare.PoolServiceID, poolSettings(declaredPools)},
			{schema.ResourcePoolClassifierProperties, workloadcompare.ClassifierServiceID, classifierSettings(declaredClassifiers)},
		} {
			request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: family.service, Kinds: []string{family.kind}, Capabilities: session.Capabilities,
				Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}, Desired: testcomparison.State(schema.NativeDesiredModel(family.kind)),
				Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedKindCoverage(snapshot.Coverage, family.kind)}}
			for name, settings := range family.declared {
				value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: family.kind, Name: name}, settings)
				c.Assert(err, qt.IsNil)
				normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: name, Representation: protocol.ValueInput_DESIRED, Value: value}}})
				c.Assert(err, qt.IsNil)
				request.Desired.Records = append(request.Desired.Records, &protocol.ComparisonRecord{Kind: family.kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
			}
			for _, value := range snapshot.NativeObjects {
				if value.Kind != family.kind {
					continue
				}
				normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "read-" + value.Owner.Name, Representation: protocol.ValueInput_OBSERVED, Value: value}}})
				c.Assert(err, qt.IsNil)
				request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: family.kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
			}
			compared, err := client.CompareModels(ctx, request)
			c.Assert(err, qt.IsNil)
			c.Assert(compared.Undecided, qt.HasLen, 0)
			for _, change := range compared.Changes {
				change.Value.Id = family.service + "/" + change.Value.Id
				planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
			}
		}
		if len(planning.Changes) == 0 {
			return nil
		}
		planned, err := client.PlanFeatures(ctx, planning)
		c.Assert(err, qt.IsNil)
		c.Assert(planned.Diagnostics, qt.HasLen, 0)
		rendering := testplanning.RenderRequest(planned, session.Capabilities)
		rendering.Form = form
		rendered, err := client.Render(ctx, rendering)
		c.Assert(err, qt.IsNil)
		return rendered.Units
	}
	execute := func(expected protocol.Execution_Outcome, units ...*protocol.Unit) {
		c.Helper()
		result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "workload", Units: units})
		if expected == protocol.Execution_APPLIED {
			c.Assert(err, qt.IsNil)
		} else {
			c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		}
		c.Assert(result.Outcome, qt.Equals, expected)
	}
	held := func() (map[string]heldPool, map[string]heldClassifier) {
		c.Helper()
		return readHeldWorkload(c, ctx, opened.DB, database, pools, classifiers)
	}

	created := map[string]workload.PoolSpec{poolName: {ConcurrentQueryLimit: new(int32(2)), QueueSize: new(int32(4))}}
	routes := map[string]workload.ClassifierSpec{first: {ResourcePool: poolName, Rank: low}, second: {ResourcePool: "default", MemberName: "etl", Rank: high}}
	units := plan(protocol.RenderRequest_ANY, created, routes)
	c.Assert(units, qt.HasLen, 1)
	execute(protocol.Execution_APPLIED, units...)
	heldPools, heldClassifiers := held()
	c.Assert(heldPools, qt.DeepEquals, map[string]heldPool{poolName: {ConcurrentQueryLimit: 2, QueueSize: 4, ResourceWeight: -1}})
	c.Assert(heldClassifiers, qt.DeepEquals, map[string]heldClassifier{first: {Pool: poolName, Rank: low}, second: {Pool: "default", Member: "etl", Rank: high}})
	c.Assert(plan(protocol.RenderRequest_ANY, created, routes), qt.HasLen, 0)

	// Two classifiers trade ranks and the pool loses its queue, written as
	// the text a migration file holds and run as one query.
	changed := map[string]workload.PoolSpec{poolName: {ConcurrentQueryLimit: new(int32(5))}}
	swapped := map[string]workload.ClassifierSpec{first: {ResourcePool: poolName, Rank: high}, second: {ResourcePool: poolName, Rank: low}}
	units = plan(protocol.RenderRequest_TEXT, changed, swapped)
	c.Assert(units, qt.HasLen, 1)
	c.Assert(units[0].GetSql(), qt.Not(qt.Equals), "")
	execute(protocol.Execution_APPLIED, units...)
	heldPools, heldClassifiers = held()
	c.Assert(heldPools, qt.DeepEquals, map[string]heldPool{poolName: {ConcurrentQueryLimit: 5, QueueSize: -1, ResourceWeight: -1}})
	c.Assert(heldClassifiers, qt.DeepEquals, map[string]heldClassifier{first: {Pool: poolName, Rank: high}, second: {Pool: poolName, Rank: low}})
	c.Assert(plan(protocol.RenderRequest_ANY, changed, swapped), qt.HasLen, 0)

	// A change planned against a pool another writer changed since is
	// refused before any statement runs.
	wider := map[string]workload.PoolSpec{poolName: {ConcurrentQueryLimit: new(int32(7))}}
	stale := plan(protocol.RenderRequest_ANY, wider, swapped)
	c.Assert(execFixtureSchema(ctx, opened.DB, "ALTER RESOURCE POOL `"+poolName+"` SET (CONCURRENT_QUERY_LIMIT = 9)"), qt.IsNil)
	execute(protocol.Execution_NOT_STARTED, stale...)
	heldPools, _ = held()
	c.Assert(heldPools[poolName].ConcurrentQueryLimit, qt.Equals, int32(9))
	execute(protocol.Execution_APPLIED, plan(protocol.RenderRequest_ANY, wider, swapped)...)
	heldPools, _ = held()
	c.Assert(heldPools[poolName].ConcurrentQueryLimit, qt.Equals, int32(7))

	// A source that leaves them out keeps them: no change is planned.
	c.Assert(plan(protocol.RenderRequest_ANY, nil, nil), qt.HasLen, 0)

	// A removal names each object: the classifiers go before their pool.
	removal, err := schema.CaptureWorkloadOperation("workload/000000", "", schema.WorkloadChanges{
		Pools: []workload.PoolChange{{Name: poolName, Before: new(wider[poolName])}},
		Classifiers: []workload.ClassifierChange{
			{Name: first, Before: new(swapped[first])},
			{Name: second, Before: new(swapped[second])},
		},
	})
	c.Assert(err, qt.IsNil)
	rendered, err := client.Render(ctx, &protocol.RenderRequest{Engine: "ydb", Capabilities: session.Capabilities, Changes: []*protocol.Change{{Id: removal.Id,
		Object: proto.CloneOf(removal.Value.Owner), Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: removal}}}}})
	c.Assert(err, qt.IsNil)
	execute(protocol.Execution_APPLIED, rendered.Units...)
	heldPools, heldClassifiers = held()
	c.Assert(heldPools, qt.HasLen, 0)
	c.Assert(heldClassifiers, qt.HasLen, 0)
}

func poolSettings(specs map[string]workload.PoolSpec) map[string]proto.Message {
	result := map[string]proto.Message{}
	for name, spec := range specs {
		result[name] = schema.PoolSettings(spec)
	}
	return result
}

func classifierSettings(specs map[string]workload.ClassifierSpec) map[string]proto.Message {
	result := map[string]proto.Message{}
	for name, spec := range specs {
		result[name] = schema.ClassifierSettings(spec)
	}
	return result
}

// capturedKindCoverage transfers an inspection's coverage of one kind into
// the comparison's terms, as the host does.
func capturedKindCoverage(coverage *protocol.ModelCoverage, kind string) *protocol.ComparisonCoverage {
	result := &protocol.ComparisonCoverage{}
	part := func(value *protocol.NamespaceComponent) *protocol.ComparisonPart {
		if value == nil {
			return nil
		}
		return &protocol.ComparisonPart{Source: value.Source, Normalized: value.Normalized}
	}
	for _, record := range coverage.Kinds {
		if record.Model.Kind != kind {
			continue
		}
		source := &protocol.ComparisonKnowledgeSource{Outside: proto.CloneOf(record.Knowledge)}
		for _, region := range record.Regions {
			source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Database: part(region.Scope.Database), Catalog: part(region.Scope.Catalog), Schema: part(region.Scope.Schema)},
				Knowledge: proto.CloneOf(region.Knowledge)})
		}
		result.Kinds = append(result.Kinds, &protocol.ComparisonKindCoverage{Model: proto.CloneOf(record.Model), Sources: []*protocol.ComparisonKnowledgeSource{source}})
	}
	for _, subject := range coverage.Subjects {
		if subject.Kind == kind {
			result.Subjects = append(result.Subjects, &protocol.ComparisonSubjectCoverage{Kind: subject.Kind, Subject: testplanning.Subject(subject.NormalizedSubject), Knowledge: proto.CloneOf(subject.Knowledge)})
		}
	}
	return result
}
