package schema_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	c := testcheck.New(t)
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
		// Ported from the host's TestDefaultPoolSettingsInvalidValuesHaveUnknownEffects and
		// TestPoolCodecsRefuseInvalidModelsAndUnknownEffects: a limit is a finite percentage.
		{name: "an infinite limit on the pool default", owner: poolOwner("default"), settings: schema.PoolSettings(workload.PoolSpec{ResourceWeight: new(math.Inf(1))}),
			wantErr: `resource pool "default": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		{name: "an infinite limit", owner: poolOwner("p"), settings: schema.PoolSettings(workload.PoolSpec{QueryCPULimitPercentPerNode: new(math.Inf(1))}),
			wantErr: `resource pool "p": QUERY_CPU_LIMIT_PERCENT_PER_NODE takes a percentage from 0 to 100`},
		{name: "a limit that is not a number", owner: poolOwner("p"), settings: schema.PoolSettings(workload.PoolSpec{ResourceWeight: new(math.NaN())}),
			wantErr: `resource pool "p": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		// Ported from the host's TestWorkloadIdentityRefusesPathsAndForeignKinds.
		{name: "an empty name", owner: poolOwner(""), settings: &native.ResourcePoolSettings{},
			wantErr: "workload objects require exact database-scoped YDB identities"},
		{name: "a name with a slash", owner: poolOwner("dir/pool"), settings: &native.ResourcePoolSettings{},
			wantErr: "workload objects require exact database-scoped YDB identities"},
		{name: "a name that is not ASCII", owner: poolOwner("пул"), settings: &native.ResourcePoolSettings{},
			wantErr: "workload objects require exact database-scoped YDB identities"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := schema.CaptureWorkloadIntent(test.owner, test.settings)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

func TestWorkloadChange_HappyPath(t *testing.T) {
	c := testcheck.New(t)
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
			c := testcheck.New(t)
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
	c := testcheck.New(t)
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
			c := testcheck.New(t)
			change, err := schema.CaptureResourcePoolChange("change-1", poolOwner(test.owner), test.before, test.after)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

func TestWorkloadOperation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
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
	c := testcheck.New(t)
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
		// Ported from the host's TestDefaultPoolSettingsRefusesMalformedWire,
		// TestWorkloadCodecsRejectIncompleteOrAmbiguousWire and TestPoolCodecsRefuseInvalidModelsAndUnknownEffects.
		{name: "a setting in another case", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"Resource_Weight":2}}],"classifiers":[]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "a setting twice", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"resourceWeight":2,"resourceWeight":3}}],"classifiers":[]}`),
			wantErr: `invalid model JSON: invalid JSON: duplicate object key "resourceWeight"`},
		{name: "a setting under both of its spellings", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"resource_weight":2,"resourceWeight":3}}],"classifiers":[]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "a name twice", value: payload(valid.Value, `{"pools":[{"name":"batch","name":"other","after":{}}],"classifiers":[]}`),
			wantErr: `invalid model JSON: invalid JSON: duplicate object key "name"`},
		{name: "a name that is not UTF-8", value: payload(valid.Value, "{\"pools\":[{\"name\":\"\xff\",\"after\":{}}],\"classifiers\":[]}"),
			wantErr: `invalid model JSON: JSON is not valid UTF-8`},
		{name: "a null limit", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"resourceWeight":null}}],"classifiers":[]}`), wantErr: `null YDB model setting`},
		{name: "a fractional count", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"concurrentQueryLimit":0.5}}],"classifiers":[]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "a count past 32 bits", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"concurrentQueryLimit":2147483648}}],"classifiers":[]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "a concurrency limit on the pool default", value: payload(valid.Value, `{"pools":[{"name":"default","before":{},"after":{"concurrentQueryLimit":0}}],"classifiers":[]}`),
			wantErr: `resource pool "default": the pool default takes no concurrent_query_limit: .*`},
		{name: "an infinite limit", value: payload(valid.Value, `{"pools":[{"name":"batch","after":{"resourceWeight":"Infinity"}}],"classifiers":[]}`),
			wantErr: `resource pool "batch": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		{name: "a limit held before that is not a number", value: payload(valid.Value, `{"pools":[{"name":"batch","before":{"resourceWeight":"NaN"},"after":{}}],"classifiers":[]}`),
			wantErr: `resource pool "batch": RESOURCE_WEIGHT takes a percentage from 0 to 100`},
		{name: "a null rank", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"p","rank":null}}]}`), wantErr: `null YDB model setting`},
		{name: "a rank past 64 bits", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"p","rank":9223372036854775808}}]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "a fractional rank", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"p","rank":1.5}}]}`),
			wantErr: `invalid desired native YDB JSON`},
		{name: "an empty pool name", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"","rank":"1"}}]}`),
			wantErr: `resource pool classifier "a" requires a pool and a nonnegative rank`},
		{name: "a null member", value: payload(valid.Value, `{"pools":[],"classifiers":[{"name":"a","after":{"resourcePool":"p","memberName":null}}]}`), wantErr: `null YDB model setting`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := schema.NormalizeWorkloadOperation(&protocol.ValueInput{Id: "x", Representation: protocol.ValueInput_OPERATION, Value: test.value})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// A pool's name may hold a dot and a classifier's a backtick and a quote, and
// two names that differ only in case name two pools, which one operation
// creates side by side. Ported from the host's
// TestWorkloadNamesAreDatabaseScopedAndCaseSensitive.
func TestWorkloadNames_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	pool := declaredPool(c, "Pool.v1", workload.PoolSpec{})
	c.Assert(pool.Owner.Name, qt.Equals, "Pool.v1")
	classifier := declaredClassifier(c, "Team`'", workload.ClassifierSpec{ResourcePool: "Pool.v1"})
	c.Assert(classifier.Owner.Name, qt.Equals, "Team`'")

	operation, err := schema.CaptureWorkloadOperation("workload/000000", "", schema.WorkloadChanges{
		Pools: []workload.PoolChange{{Name: "Pool", After: &workload.PoolSpec{}}, {Name: "pool", After: &workload.PoolSpec{}}}})

	c.Assert(err, qt.IsNil)
	decoded, err := schema.DecodeWorkloadOperation(operation.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(decoded.Pools, qt.DeepEquals, []workload.PoolChange{{Name: "Pool", After: &workload.PoolSpec{}}, {Name: "pool", After: &workload.PoolSpec{}}})
}

// A pool's or a classifier's model refuses a payload that would decode to
// another object than the one written, in either representation: no object,
// a key in another case, a key it does not hold, one key twice or under both
// of its spellings, a value past its type, a count past 32 bits, a queue with
// nothing to wait for, and a classifier with no pool. A declaration refuses a
// null too. Ported from the host's TestModelCodecsRefuseAmbiguousOrInvalidWire,
// TestPoolCodecsRefuseInvalidLimits and
// TestClassifierCodecsRequireExplicitCompleteRouting.
func TestWorkloadModels_RefuseAForgedPayload(t *testing.T) {
	desired, observed := protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED
	for _, test := range []struct {
		name           string
		owner          *protocol.Reference
		representation protocol.ValueInput_Representation
		payload        string
		wantErr        string
	}{
		{"a declared null", poolOwner("p"), desired, `null`, `desired native YDB value requires an object`},
		{"an observed null", poolOwner("p"), observed, `null`, `invalid YDB observation JSON`},
		{"a declared key in another case", poolOwner("p"), desired, `{"ResourceWeight":12}`, `invalid desired native YDB JSON`},
		{"an observed key in another case", classifierOwner("c"), observed, `{"resourcePool":"p","rank":"0","Rank":"1"}`, `invalid YDB observation JSON`},
		{"a declared extra key", classifierOwner("c"), desired, `{"resourcePool":"p","extra":true}`, `invalid desired native YDB JSON`},
		{"an observed extra key", poolOwner("p"), observed, `{"extra":true}`, `invalid YDB observation JSON`},
		{"a declared key twice", classifierOwner("c"), desired, `{"resourcePool":"p","rank":0,"rank":1}`, `invalid model JSON: invalid JSON: duplicate object key "rank"`},
		{"an observed key twice", poolOwner("p"), observed, `{"resourceWeight":2,"resourceWeight":3}`, `invalid YDB observation JSON`},
		{"an observed key under both of its spellings", poolOwner("p"), observed, `{"resource_weight":2,"resourceWeight":3}`, `invalid YDB observation JSON`},
		{"a declared null limit", poolOwner("p"), desired, `{"concurrentQueryLimit":null}`, `null YDB model setting`},
		{"an observed count past 32 bits", poolOwner("p"), observed, `{"concurrentQueryLimit":2147483648}`, `invalid YDB observation JSON`},
		{"an observed limit past a double", poolOwner("p"), observed, `{"resourceWeight":1e400}`, `invalid YDB observation JSON`},
		{"a declared zero queue", poolOwner("p"), desired, `{"queueSize":0}`, `resource pool "p": a queue needs .*`},
		{"an observed zero queue", poolOwner("p"), observed, `{"queueSize":0}`, `resource pool "p": a queue needs .*`},
		{"a declared classifier with no pool", classifierOwner("c"), desired, `{}`, `resource pool classifier "c" requires a pool and a nonnegative rank`},
		{"an observed empty pool name", classifierOwner("c"), observed, `{"resourcePool":"","rank":"0"}`, `resource pool classifier "c" requires a pool and a nonnegative rank`},
		{"a declared null rank", classifierOwner("c"), desired, `{"resourcePool":"p","rank":null}`, `null YDB model setting`},
		{"an observed rank past 64 bits", classifierOwner("c"), observed, `{"resourcePool":"p","rank":"9223372036854775808"}`, `invalid YDB observation JSON`},
		{"a declared fractional rank", classifierOwner("c"), desired, `{"resourcePool":"p","rank":1.5}`, `invalid desired native YDB JSON`},
		{"a declared null member", classifierOwner("c"), desired, `{"resourcePool":"p","memberName":null}`, `null YDB model setting`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := normalizeIn(test.representation)(forgedValue(test.owner.Kind, test.representation, test.owner, test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

// payload is value with another payload.
func payload(value *protocol.Envelope, raw string) *protocol.Envelope {
	changed := proto.CloneOf(value)
	changed.Payload = []byte(raw)
	return changed
}
