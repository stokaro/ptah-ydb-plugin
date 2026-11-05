package nativeapi_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"testing/synctest"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// fakeWorkload holds a database's pools and classifiers and applies the
// statements a test expects, as YDB would.
type fakeWorkload struct {
	held     nativeapi.HeldWorkload
	after    nativeapi.HeldWorkload
	executed []string
	fail     error
}

func (f *fakeWorkload) bind() *nativeapi.Workload {
	return &nativeapi.Workload{
		Read: func(context.Context) (nativeapi.HeldWorkload, error) {
			held := nativeapi.HeldWorkload{Pools: maps.Clone(f.held.Pools), Classifiers: maps.Clone(f.held.Classifiers)}
			return held, nil
		},
		Execute: func(_ context.Context, statement string) error {
			f.executed = append(f.executed, statement)
			if f.fail != nil {
				return f.fail
			}
			f.held = f.after
			return nil
		},
	}
}

func rankSwap() schema.WorkloadChanges {
	return schema.WorkloadChanges{
		Pools: []workload.PoolChange{{Name: "batch", After: &workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}}},
		Classifiers: []workload.ClassifierChange{
			{Name: "alpha", Before: &workload.ClassifierSpec{ResourcePool: "default", Rank: 10}, After: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 20}},
			{Name: "beta", Before: &workload.ClassifierSpec{ResourcePool: "default", Rank: 20}, After: &workload.ClassifierSpec{ResourcePool: "batch", Rank: 10}},
		},
	}
}

func heldBefore() nativeapi.HeldWorkload {
	return nativeapi.HeldWorkload{Pools: map[string]workload.PoolSpec{"default": {}},
		Classifiers: map[string]workload.ClassifierSpec{"alpha": {ResourcePool: "default", Rank: 10}, "beta": {ResourcePool: "default", Rank: 20}}}
}

func heldAfter() nativeapi.HeldWorkload {
	return nativeapi.HeldWorkload{Pools: map[string]workload.PoolSpec{"default": {}, "batch": {ConcurrentQueryLimit: new(int32(2))}},
		Classifiers: map[string]workload.ClassifierSpec{"alpha": {ResourcePool: "batch", Rank: 20}, "beta": {ResourcePool: "batch", Rank: 10}}}
}

func workloadOperation(c *qt.C, changes schema.WorkloadChanges) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureWorkloadOperation("workload/000000", "", changes)
	c.Assert(err, qt.IsNil)
	return value.Value
}

// The operation checks that the database holds what the plan read, runs the
// statements in the host's order, one query each, and reads the result back.
func TestWorkloadApply_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fake := &fakeWorkload{held: heldBefore(), after: heldAfter()}
	checked := false
	dispatched, err := fake.bind().Apply(c.Context(), "/local", workloadOperation(c, rankSwap()), func(context.Context) error { checked = true; return nil })
	c.Assert(err, qt.IsNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(checked, qt.IsTrue)
	c.Assert(fake.executed, qt.DeepEquals, []string{
		"DROP RESOURCE POOL CLASSIFIER `alpha`;",
		"DROP RESOURCE POOL CLASSIFIER `beta`;",
		"CREATE RESOURCE POOL `batch` WITH (CONCURRENT_QUERY_LIMIT = 2);",
		"CREATE RESOURCE POOL CLASSIFIER `alpha` WITH (RESOURCE_POOL = 'batch', RANK = 20);",
		"CREATE RESOURCE POOL CLASSIFIER `beta` WITH (RESOURCE_POOL = 'batch', RANK = 10);",
	})
}

// A database that changed since the plan read it is not written, a failed
// statement stops the operation, and a result the read does not confirm is
// reported as dispatched.
func TestWorkloadApply_FailurePath(t *testing.T) {
	moved := heldBefore()
	moved.Classifiers["alpha"] = workload.ClassifierSpec{ResourcePool: "default", Rank: 11}
	lost := heldAfter()
	delete(lost.Pools, "batch")
	refused := errors.New("Classifier with rank 20 already exists")
	for _, test := range []struct {
		name           string
		fake           *fakeWorkload
		database       string
		wantDispatched bool
		wantExecuted   int
		wantErr        string
	}{
		{name: "a classifier moved by another writer", fake: &fakeWorkload{held: moved, after: heldAfter()}, database: "/local",
			wantErr: `resource pool classifier "alpha" changed since the plan read it`},
		{name: "a statement YDB refuses", fake: &fakeWorkload{held: heldBefore(), fail: refused}, database: "/local", wantDispatched: true, wantExecuted: 1,
			wantErr: "Classifier with rank 20 already exists"},
		{name: "a result the read does not confirm", fake: &fakeWorkload{held: heldBefore(), after: lost}, database: "/local", wantDispatched: true, wantExecuted: 5,
			wantErr: `resource pool "batch" was not confirmed: the database holds it otherwise`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dispatched, err := test.fake.bind().Apply(c.Context(), test.database, workloadOperation(c, rankSwap()), func(context.Context) error { return nil })
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(dispatched, qt.Equals, test.wantDispatched)
			c.Assert(test.fake.executed, qt.HasLen, test.wantExecuted)
		})
	}
}

// An operation of another database, or a lock lost before the first
// statement, writes nothing.
func TestWorkloadApply_RefusesBeforeWriting(t *testing.T) {
	c := testcheck.New(t)
	foreign, err := schema.CaptureWorkloadOperation("workload/000000", "/other", rankSwap())
	c.Assert(err, qt.IsNil)
	fake := &fakeWorkload{held: heldBefore(), after: heldAfter()}
	dispatched, err := fake.bind().Apply(c.Context(), "/local", foreign.Value, func(context.Context) error { return nil })
	c.Assert(err, qt.ErrorMatches, "workload operation targets another database")
	c.Assert(dispatched, qt.IsFalse)
	dispatched, err = fake.bind().Apply(c.Context(), "/local", workloadOperation(c, rankSwap()), func(context.Context) error { return errors.New("lock lost") })
	c.Assert(err, qt.ErrorMatches, "lock lost")
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(fake.executed, qt.HasLen, 0)
}

// errRouted is what YDB answers a query the workload manager routes to a
// pool that is gone, as the SDK spells it.
var errRouted = errors.New("operation/NOT_FOUND (code = 400140, issues = [{'Failed to resolve pool id batch' [{'Resource pool batch not found or you " +
	"don't have access permissions'}]},{'Query failed during adding/waiting in workload pool '}])")

// stillRouted binds fake, as a session does, with a read through
// nativeapi.RoutedRead that YDB refuses, as routed to a gone pool, the given
// number of times once a statement ran, and reads the fake after that.
func stillRouted(fake *fakeWorkload, refusals int) *nativeapi.Workload {
	bound := fake.bind()
	read := bound.Read
	bound.Read = nativeapi.RoutedRead(func(ctx context.Context) (nativeapi.HeldWorkload, error) {
		if len(fake.executed) > 0 && refusals > 0 {
			refusals--
			return nativeapi.HeldWorkload{}, errRouted
		}
		return read(ctx)
	})
	return bound
}

// The read that confirms an operation is sent again while YDB refuses it as
// routed to a pool that is gone: YDB refuses such a read before it runs it,
// and a read changes nothing. Without that, the removal of a classifier that
// routes every query to its pool, and of the pool, is reported unconfirmed
// whenever the workload manager's cache still holds the classifier.
func TestWorkloadApply_WaitsOutAPoolStillRouted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		fake := &fakeWorkload{held: heldBefore(), after: heldAfter()}

		dispatched, err := stillRouted(fake, 3).Apply(c.Context(), "/local", workloadOperation(c, rankSwap()), func(context.Context) error { return nil })

		c.Assert(err, qt.IsNil)
		c.Assert(dispatched, qt.IsTrue)
		c.Assert(fake.executed, qt.HasLen, 5)
	})
}

// A routing refusal that outlasts nativeapi.PoolRoutingBudget is returned,
// and the operation is reported dispatched, since its statements ran.
func TestWorkloadApply_StopsWaitingForAPoolStillRouted_FailurePath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		fake := &fakeWorkload{held: heldBefore(), after: heldAfter()}
		started := time.Now()

		dispatched, err := stillRouted(fake, 1<<30).Apply(c.Context(), "/local", workloadOperation(c, rankSwap()), func(context.Context) error { return nil })

		c.Assert(err, qt.ErrorIs, errRouted)
		c.Assert(dispatched, qt.IsTrue)
		c.Assert(time.Since(started) >= nativeapi.PoolRoutingBudget, qt.IsTrue)
	})
}

// Only YDB's routing refusal is sent again: another failed read is returned
// at once.
func TestPoolUnresolved(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "a query routed to a gone pool", err: fmt.Errorf("read YDB resource pools: %w", errRouted), want: true},
		{name: "another refusal", err: errors.New("operation/SCHEME_ERROR (issues = [{'Path does not exist'}])")},
		{name: "no error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(nativeapi.PoolUnresolved(test.err), qt.Equals, test.want)
		})
	}
}
