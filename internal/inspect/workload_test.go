package inspect_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// snapshot is what Read returns for a database holding nothing.
func snapshot() *protocol.Schema {
	result := &protocol.Schema{Complete: true, Engine: "ydb", Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
	for _, model := range schema.ObservedModels() {
		result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ModelKindCoverage{Model: model,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Knowledge is limited to returned subjects and fully listed directories"}})
	}
	return result
}

func workloadKnowledge(read *protocol.Schema) map[string]*protocol.ModelKnowledge {
	knowledge := map[string]*protocol.ModelKnowledge{}
	for _, record := range read.Coverage.Kinds {
		if schema.IsWorkloadObject(record.Model.Kind) {
			knowledge[record.Model.Kind] = record.Knowledge
		}
	}
	return knowledge
}

// A read of the system views returns every pool and classifier under the
// database and establishes the absence of every other one.
func TestWorkload_HappyPath(t *testing.T) {
	c := qt.New(t)
	read := snapshot()
	err := inspect.Workload(c.Context(), read, "/local", nil, func(context.Context) (nativeapi.HeldWorkload, error) {
		return nativeapi.HeldWorkload{Pools: map[string]workload.PoolSpec{"default": {}, "batch": {ConcurrentQueryLimit: new(int32(2))}},
			Classifiers: map[string]workload.ClassifierSpec{"etl": {ResourcePool: "batch", Rank: 1}}}, nil
	}, "")
	c.Assert(err, qt.IsNil)
	var names []string
	for _, value := range read.NativeObjects {
		c.Assert(value.Owner.Database, qt.Equals, "/local")
		names = append(names, value.Kind+" "+value.Owner.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{schema.ResourcePoolProperties + " batch", schema.ResourcePoolProperties + " default", schema.ResourcePoolClassifierProperties + " etl"})
	c.Assert(read.Coverage.Subjects, qt.HasLen, 3)
	for _, record := range workloadRecords(read) {
		c.Assert(record.Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
		c.Assert(record.Regions, qt.HasLen, 1)
		c.Assert(record.Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
		c.Assert(proto.Equal(record.Regions[0].Scope, &protocol.ModelNamespace{
			Database: &protocol.NamespaceComponent{Source: "/local", Normalized: "/local"}, Catalog: &protocol.NamespaceComponent{}, Schema: &protocol.NamespaceComponent{},
		}), qt.IsTrue)
	}
}

func TestWorkload_EmptyReadEstablishesOnlyDatabaseRootAbsence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		selectors []string
	}{
		{"unfiltered", nil},
		{"explicit root", []string{"/"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			read := snapshot()
			c.Assert(inspect.Workload(c.Context(), read, "/tenant", test.selectors, func(context.Context) (nativeapi.HeldWorkload, error) {
				return nativeapi.HeldWorkload{}, nil
			}, ""), qt.IsNil)
			c.Assert(read.NativeObjects, qt.HasLen, 0)
			for _, record := range workloadRecords(read) {
				c.Assert(record.Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
				c.Assert(record.Regions, qt.HasLen, 1)
				c.Assert(record.Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
				c.Assert(proto.Equal(record.Regions[0].Scope, &protocol.ModelNamespace{
					Database: &protocol.NamespaceComponent{Source: "/tenant", Normalized: "/tenant"}, Catalog: &protocol.NamespaceComponent{}, Schema: &protocol.NamespaceComponent{},
				}), qt.IsTrue)
			}
		})
	}
}

// A target that keeps no resource pools lists nothing in its views, which
// proves no absence, so both kinds stay uninspected for the reason given.
func TestWorkload_Unread(t *testing.T) {
	c := qt.New(t)
	read := snapshot()
	c.Assert(inspect.Workload(c.Context(), read, "/local", nil, nil, "the flag is off"), qt.IsNil)
	c.Assert(read.NativeObjects, qt.HasLen, 0)
	knowledge := workloadKnowledge(read)
	c.Assert(knowledge, qt.HasLen, 2)
	for _, record := range knowledge {
		c.Assert(record.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
		c.Assert(record.Reason, qt.Equals, "the flag is off")
	}
}

func TestWorkload_FailurePath(t *testing.T) {
	c := qt.New(t)
	read := snapshot()
	err := inspect.Workload(c.Context(), read, "/local", nil, func(context.Context) (nativeapi.HeldWorkload, error) {
		return nativeapi.HeldWorkload{}, errors.New("read YDB resource pools: refused")
	}, "")
	c.Assert(err, qt.ErrorMatches, "read YDB resource pools: refused")
	c.Assert(read.NativeObjects, qt.HasLen, 0)
}

// A read of selected directories cannot report what belongs to the whole
// database, so it does not read the views; a read that selects the root
// reports them for the root namespace alone.
func TestWorkload_Selectors(t *testing.T) {
	c := qt.New(t)
	read := func(context.Context) (nativeapi.HeldWorkload, error) {
		return nativeapi.HeldWorkload{Pools: map[string]workload.PoolSpec{"default": {}}}, nil
	}
	directory := snapshot()
	c.Assert(inspect.Workload(c.Context(), directory, "/local", []string{"app"}, func(context.Context) (nativeapi.HeldWorkload, error) {
		return nativeapi.HeldWorkload{}, errors.New("the views are not read")
	}, ""), qt.IsNil)
	c.Assert(directory.NativeObjects, qt.HasLen, 0)
	for _, record := range workloadKnowledge(directory) {
		c.Assert(record.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
		c.Assert(record.Reason, qt.Matches, `A read of selected directories does not report .*; select / to read them`)
	}
	root := snapshot()
	c.Assert(inspect.Workload(c.Context(), root, "/local", []string{"app", "/"}, read, ""), qt.IsNil)
	c.Assert(root.NativeObjects, qt.HasLen, 1)
	for kind, record := range workloadRecords(root) {
		c.Assert(record.Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED, qt.Commentf(kind))
		c.Assert(record.Regions, qt.HasLen, 1)
		c.Assert(record.Regions[0].Scope.Schema.Normalized, qt.Equals, "")
		c.Assert(record.Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
	}
}

// workloadRecords are a read's coverage records of the workload kinds.
func workloadRecords(read *protocol.Schema) map[string]*protocol.ModelKindCoverage {
	records := map[string]*protocol.ModelKindCoverage{}
	for _, record := range read.Coverage.Kinds {
		if schema.IsWorkloadObject(record.Model.Kind) {
			records[record.Model.Kind] = record
		}
	}
	return records
}
