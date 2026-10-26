package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func poolOwner(name string) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: name}
}

func classifierOwner(name string) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: name}
}

// readPool is a pool as inspection captures it from /local.
func readPool(c *qt.C, name string, spec workload.PoolSpec) *protocol.Envelope {
	c.Helper()
	owner := poolOwner(name)
	owner.Database = "/local"
	value, err := schema.CaptureWorkloadObservation(owner, schema.PoolSettings(spec))
	c.Assert(err, qt.IsNil)
	value.Owner.Database = ""
	return value
}

func declaredPool(c *qt.C, name string, spec workload.PoolSpec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(poolOwner(name), schema.PoolSettings(spec))
	c.Assert(err, qt.IsNil)
	return value
}

func readClassifier(c *qt.C, name string, spec workload.ClassifierSpec) *protocol.Envelope {
	c.Helper()
	owner := classifierOwner(name)
	owner.Database = "/local"
	value, err := schema.CaptureWorkloadObservation(owner, schema.ClassifierSettings(spec))
	c.Assert(err, qt.IsNil)
	value.Owner.Database = ""
	return value
}

func declaredClassifier(c *qt.C, name string, spec workload.ClassifierSpec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(classifierOwner(name), schema.ClassifierSettings(spec))
	c.Assert(err, qt.IsNil)
	return value
}

func TestWorkloadModels_HappyPath(t *testing.T) {
	c := qt.New(t)
	spec := workload.PoolSpec{ConcurrentQueryLimit: new(int32(0)), QueryMemoryLimitPercentPerNode: new(12.5)}
	declared := declaredPool(c, "batch", spec)
	read, err := schema.WorkloadPool(declared)
	c.Assert(err, qt.IsNil)
	c.Assert(read, qt.DeepEquals, spec)
	c.Assert(string(declared.Payload), qt.Equals, `{"concurrentQueryLimit":0,"queryMemoryLimitPercentPerNode":12.5}`)

	converted, err := schema.ConvertNative(readPool(c, "batch", spec), protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(converted, declared), qt.IsTrue)

	routed := workload.ClassifierSpec{ResourcePool: "batch", MemberName: "etl", Rank: 0}
	classifier, err := schema.WorkloadClassifier(declaredClassifier(c, "etl", routed))
	c.Assert(err, qt.IsNil)
	c.Assert(classifier, qt.Equals, routed)

	// The pool default takes the settings YDB lets it change.
	_, err = schema.CaptureWorkloadIntent(poolOwner("default"), schema.PoolSettings(workload.PoolSpec{ResourceWeight: new(30.0)}))
	c.Assert(err, qt.IsNil)
}

func TestWorkloadModels_FailurePath(t *testing.T) {
	inDirectory := poolOwner("batch")
	inDirectory.Schema = "app"
	for _, test := range []struct {
		name     string
		owner    *protocol.Reference
		settings proto.Message
		wantErr  string
	}{
		{name: "a pool in a directory", owner: inDirectory, settings: &native.ResourcePoolSettings{},
			wantErr: "workload objects require exact database-scoped YDB identities"},
		{name: "a name YDB does not take", owner: poolOwner("a b"), settings: &native.ResourcePoolSettings{},
			wantErr: "workload objects require exact database-scoped YDB identities"},
		{name: "a limit on the pool default", owner: poolOwner("default"), settings: schema.PoolSettings(workload.PoolSpec{ConcurrentQueryLimit: new(int32(1))}),
			wantErr: `resource pool "default": the pool default takes no concurrent_query_limit: .*`},
		{name: "a percentage out of range", owner: poolOwner("p"), settings: schema.PoolSettings(workload.PoolSpec{ResourceWeight: new(101.0)}),
			wantErr: `resource pool "p": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		{name: "a queue with nothing to wait for", owner: poolOwner("p"), settings: schema.PoolSettings(workload.PoolSpec{QueueSize: new(int32(1))}),
			wantErr: `resource pool "p": a queue needs .*`},
		{name: "a classifier with no pool", owner: classifierOwner("c"), settings: &native.ResourcePoolClassifierSettings{Rank: 1},
			wantErr: `resource pool classifier "c" requires a pool and a nonnegative rank`},
		{name: "a classifier with a negative rank", owner: classifierOwner("c"), settings: &native.ResourcePoolClassifierSettings{ResourcePool: "p", Rank: -1},
			wantErr: `resource pool classifier "c" requires a pool and a nonnegative rank`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, err := schema.CaptureWorkloadIntent(test.owner, test.settings)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

func TestWorkloadChange_HappyPath(t *testing.T) {
	c := qt.New(t)
	limited := workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}
	for _, test := range []struct {
		name          string
		before, after *protocol.Envelope
		classifier    bool
		impact        protocol.ChangeSemantics_Impact
		reason        string
	}{
		{name: "a new pool", after: declaredPool(c, "batch", limited), impact: protocol.ChangeSemantics_ADDITIVE,
			reason: "CREATE RESOURCE POOL adds a resource pool; it does not remove data or tighten constraints"},
		{name: "a changed pool", before: readPool(c, "batch", workload.PoolSpec{}), after: declaredPool(c, "batch", limited), impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "ALTER RESOURCE POOL changes the limits of running and queued queries"},
		{name: "a removed pool", before: readPool(c, "batch", limited), impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "DROP RESOURCE POOL runs the queries a classifier sends to the pool in the pool default"},
		{name: "a new classifier", classifier: true, after: declaredClassifier(c, "c", workload.ClassifierSpec{ResourcePool: "batch", Rank: 1}),
			impact: protocol.ChangeSemantics_BEHAVIORAL, reason: "resource pool classifier settings change which pool receives matching queries"},
		{name: "a removed classifier", classifier: true, before: readClassifier(c, "c", workload.ClassifierSpec{ResourcePool: "batch", Rank: 1}),
			impact: protocol.ChangeSemantics_BEHAVIORAL, reason: "DROP RESOURCE POOL CLASSIFIER sends its member's queries to another classifier's pool or to the pool default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			capture, owner := schema.CaptureResourcePoolChange, poolOwner("batch")
			if test.classifier {
				capture, owner = schema.CaptureResourcePoolClassifierChange, classifierOwner("c")
			}
			change, err := capture("change-1", owner, test.before, test.after)
			c.Assert(err, qt.IsNil)
			facts := change.GetChangeSemantics()
			c.Assert(facts.GetImpact(), qt.Equals, test.impact)
			c.Assert(facts.GetReason(), qt.Equals, test.reason)
			c.Assert(facts.GetAccess(), qt.Equals, protocol.ChangeSemantics_UNCHANGED)
			c.Assert(facts.GetRemovesState(), qt.IsFalse)
			again, err := schema.NormalizeWorkloadChange(&protocol.ValueInput{Id: "change-1", Representation: protocol.ValueInput_CHANGE, Value: change.Value})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(again, change), qt.IsTrue)
		})
	}
}

func TestWorkloadChange_FailurePath(t *testing.T) {
	c := qt.New(t)
	same := workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}
	for _, test := range []struct {
		name          string
		owner         string
		before, after *protocol.Envelope
		wantErr       string
	}{
		{name: "a pool that holds no change", owner: "batch", before: readPool(c, "batch", same), after: declaredPool(c, "batch", same),
			wantErr: "resource pool operands contain no change"},
		{name: "creating the pool default", owner: "default", after: declaredPool(c, "default", workload.PoolSpec{}),
			wantErr: `resource pool "default": the default pool cannot be created or dropped`},
		{name: "dropping the pool default", owner: "default", before: readPool(c, "default", workload.PoolSpec{}),
			wantErr: `resource pool "default": the default pool cannot be created or dropped`},
		{name: "no operand", owner: "batch", wantErr: "resource pool change has no operands"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change, err := schema.CaptureResourcePoolChange("change-1", poolOwner(test.owner), test.before, test.after)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

func TestWorkloadOperation_HappyPath(t *testing.T) {
	c := qt.New(t)
	changes := schema.WorkloadChanges{
		Pools: []workload.PoolChange{{Name: "batch", After: &workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}}},
		Classifiers: []workload.ClassifierChange{
			{Name: "alpha", Before: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 10}, After: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 20}},
			{Name: "beta", Before: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 20}, After: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 10}},
		},
	}
	operation, err := schema.CaptureWorkloadOperation("workload/000000", "", changes)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(operation.NormalizedOwner, schema.WorkloadOwner("")), qt.IsTrue)
	c.Assert(operation.OperationSemantics.GetImpact(), qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(operation.OperationSemantics.GetReason(), qt.Equals, "CREATE RESOURCE POOL adds a resource pool; it does not remove data or tighten constraints; "+
		"resource pool classifier settings change which pool receives matching queries")
	decoded, err := schema.DecodeWorkloadOperation(operation.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(decoded, qt.DeepEquals, changes)

	created, err := schema.CaptureWorkloadOperation("workload/000000", "/local", schema.WorkloadChanges{Pools: changes.Pools})
	c.Assert(err, qt.IsNil)
	c.Assert(created.OperationSemantics.GetImpact(), qt.Equals, protocol.ChangeSemantics_ADDITIVE)
	c.Assert(created.NormalizedOwner.GetDatabase(), qt.Equals, "/local")
}

func TestWorkloadOperation_FailurePath(t *testing.T) {
	c := qt.New(t)
	valid, err := schema.CaptureWorkloadOperation("workload/000000", "", schema.WorkloadChanges{Pools: []workload.PoolChange{{Name: "batch", After: &workload.PoolSpec{}}}})
	c.Assert(err, qt.IsNil)
	foreignOwner := proto.CloneOf(valid.Value)
	foreignOwner.Owner.Name = "other"
	for _, test := range []struct {
		name    string
		value   *protocol.Envelope
		wantErr string
	}{
		{name: "no change", value: payload(valid.Value, `{"pools":[],"classifiers":[]}`), wantErr: "workload operation requires at least one change"},
		{name: "a missing list", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{}}]}`), wantErr: "workload operation requires the keys pools and classifiers"},
		{name: "an unknown key", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{},"rank":1}],"classifiers":[]}`), wantErr: `workload operation has an unknown key "rank"`},
		{name: "a null operand", value: payload(valid.Value, `{"pools":[{"name":"batch","after":null}],"classifiers":[]}`), wantErr: `workload operation key "after" cannot be null`},
		{name: "names out of order", value: payload(valid.Value, `{"pools":[{"name":"b","after":{}},{"name":"a","after":{}}],"classifiers":[]}`),
			wantErr: "workload operation changes must name each object once, in name order"},
		{name: "a change with no operand", value: payload(valid.Value, `{"pools":[{"name":"batch"}],"classifiers":[]}`), wantErr: `workload operation change of "batch" has no operand`},
		{name: "a setting out of range", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"resourceWeight":200}}],"classifiers":[]}`),
			wantErr: `resource pool "batch": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		{name: "a rank two classifiers take", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"p","rank":"1"}},{"name":"b","after":{"resourcePool":"p","rank":"1"}}]}`),
			wantErr: `resource pool classifier "b": classifiers "a" and "b" cannot share rank 1`},
		{name: "another owner", value: foreignOwner, wantErr: "workload operation requires the database's workload owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			normalized, err := schema.NormalizeWorkloadOperation(&protocol.ValueInput{Id: "x", Representation: protocol.ValueInput_OPERATION, Value: test.value})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// payload is value with another payload.
func payload(value *protocol.Envelope, raw string) *protocol.Envelope {
	changed := proto.CloneOf(value)
	changed.Payload = []byte(raw)
	return changed
}
