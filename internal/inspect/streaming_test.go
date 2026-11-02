package inspect_test

import (
	"context"
	"errors"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// The upstream scheme enum does not yet name the server's streaming type.
const queryEntry Ydb_Scheme.Entry_Type = 26

func directory(absolute string, entries ...*Ydb_Scheme.Entry) *Ydb_Scheme.ListDirectoryResult {
	return &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: path.Base(absolute), Type: Ydb_Scheme.Entry_DIRECTORY}, Children: entries}
}

func TestStreamingInventoryReadsDotDirectoriesAndCrossDirectoryQueries(t *testing.T) {
	c := testcheck.New(t)
	tree := map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local/realm":          directory("/local/realm", &Ydb_Scheme.Entry{Name: ".queries", Type: Ydb_Scheme.Entry_DIRECTORY}, &Ydb_Scheme.Entry{Name: "inputs", Type: Ydb_Scheme.Entry_DIRECTORY}),
		"/local/realm/.queries": directory("/local/realm/.queries", &Ydb_Scheme.Entry{Name: "copy", Type: queryEntry}),
		"/local/realm/inputs":   directory("/local/realm/inputs", &Ydb_Scheme.Entry{Name: "source", Type: Ydb_Scheme.Entry_TOPIC}),
	}
	var listed, read []string
	result, err := inspect.ReadStreamingInventory(c.Context(), "/local/realm", func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		listed = append(listed, absolute)
		return tree[absolute], nil
	}, func(_ context.Context, absolute string) (*streaming.Spec, error) {
		read = append(read, absolute)
		return &streaming.Spec{Text: "SELECT * FROM `/local/realm/inputs/source`;"}, nil
	})
	c.Assert(err, qt.IsNil)
	c.Assert(listed, qt.DeepEquals, []string{"/local/realm", "/local/realm/.queries", "/local/realm/inputs"})
	c.Assert(read, qt.DeepEquals, []string{"/local/realm/.queries/copy"})
	c.Assert(result.NativeObjects, qt.HasLen, 1)
	c.Assert(result.NativeObjects[0].Owner.Schema, qt.Equals, ".queries")
	spec, err := schema.ObservedStreamingQuery(result.NativeObjects[0])
	c.Assert(err, qt.IsNil)
	c.Assert(spec.Text, qt.Equals, "SELECT * FROM `/local/realm/inputs/source`;")
	c.Assert(result.Coverage.Kinds[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
	c.Assert(result.Coverage.Kinds[0].Regions[0].Scope.Database.Normalized, qt.Equals, "/local/realm")
	c.Assert(result.Coverage.Kinds[0].Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
}

func TestStreamingInventoryProvesAbsenceWithoutEnabledReader(t *testing.T) {
	c := testcheck.New(t)
	result, err := inspect.ReadStreamingInventory(c.Context(), "/local", func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		return directory(absolute, &Ydb_Scheme.Entry{Name: "history", Type: Ydb_Scheme.Entry_TABLE}), nil
	}, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(result.NativeObjects, qt.HasLen, 0)
	c.Assert(result.Coverage.Subjects, qt.HasLen, 0)
	c.Assert(result.Coverage.Kinds[0].Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
}

func TestStreamingInventoryPreservesUnreadQueries(t *testing.T) {
	for _, test := range []struct {
		name string
		read inspect.StreamingQuery
	}{{"disabled reader", nil}, {"read error", func(context.Context, string) (*streaming.Spec, error) { return nil, errors.New("catalog unavailable") }}, {"missing settings", func(context.Context, string) (*streaming.Spec, error) { return nil, nil }}} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := inspect.ReadStreamingInventory(c.Context(), "/local", func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
				return directory(absolute, &Ydb_Scheme.Entry{Name: "copy", Type: queryEntry}), nil
			}, test.read)
			c.Assert(err, qt.IsNil)
			c.Assert(result.NativeObjects, qt.HasLen, 0)
			c.Assert(result.Coverage.Subjects, qt.HasLen, 1)
			c.Assert(result.Coverage.Subjects[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
			c.Assert(result.Coverage.Subjects[0].Knowledge.Reason, qt.Not(qt.Equals), "")
		})
	}
}

func TestStreamingInventoryRefusesIncompleteDirectoryTraversal(t *testing.T) {
	failure := errors.New("permission denied")
	for _, test := range []struct {
		name string
		root string
		list func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error)
	}{
		{"invalid root", "relative", func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error) { return nil, nil }},
		{"list error", "/local", func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error) { return nil, failure }},
		{"wrong identity", "/local", func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error) {
			return directory("/other"), nil
		}},
		{"duplicate entry", "/local", func(_ context.Context, p string) (*Ydb_Scheme.ListDirectoryResult, error) {
			return directory(p, &Ydb_Scheme.Entry{Name: "x"}, &Ydb_Scheme.Entry{Name: "x"}), nil
		}},
		{"escaped directory", "/local", func(_ context.Context, p string) (*Ydb_Scheme.ListDirectoryResult, error) {
			return directory(p, &Ydb_Scheme.Entry{Name: "../outside", Type: Ydb_Scheme.Entry_DIRECTORY}), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := inspect.ReadStreamingInventory(c.Context(), test.root, test.list, nil)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
}

func TestStreamingInventoryStopsAfterCancellation(t *testing.T) {
	c := testcheck.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	calls := 0
	result, err := inspect.ReadStreamingInventory(ctx, "/local", func(_ context.Context, p string) (*Ydb_Scheme.ListDirectoryResult, error) {
		calls++
		cancel()
		return directory(p, &Ydb_Scheme.Entry{Name: "child", Type: Ydb_Scheme.Entry_DIRECTORY}), nil
	}, nil)
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(result, qt.IsNil)
	c.Assert(calls, qt.Equals, 1)
}

// scopedSnapshot is a read of one selected directory: its streaming query
// coverage proves absence in that directory only.
func scopedSnapshot() *protocol.Schema {
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == schema.StreamingQueryProperties {
			model = candidate
		}
	}
	return &protocol.Schema{Coverage: &protocol.ModelCoverage{Kinds: []*protocol.ModelKindCoverage{{
		Model:     model,
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED},
		Regions: []*protocol.ModelNamespaceKnowledge{{
			Scope:     &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: "/local", Normalized: "/local"}, Schema: &protocol.NamespaceComponent{Source: "app", Normalized: "app"}},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		}},
	}}}}
}

func inventoryOf(c *qt.C, entries ...*Ydb_Scheme.Entry) *protocol.Schema {
	inventory, err := inspect.ReadStreamingInventory(c.Context(), "/local", func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		return directory(absolute, entries...), nil
	}, func(context.Context, string) (*streaming.Spec, error) {
		return &streaming.Spec{Text: "SELECT 1;"}, nil
	})
	c.Assert(err, qt.IsNil)
	return inventory
}

// A scoped read of a database that holds no streaming query takes the whole
// database's absence; one that holds a query elsewhere keeps its own coverage,
// and an unscoped read takes the inventory either way.
func TestScopeStreamingInventory(t *testing.T) {
	query := &Ydb_Scheme.Entry{Name: "copy", Type: queryEntry}
	for _, test := range []struct {
		name       string
		entries    []*Ydb_Scheme.Entry
		scoped     bool
		wantSchema string
		wantValues int
	}{
		{"scoped, no query anywhere", nil, true, "", 0},
		{"scoped, a query elsewhere", []*Ydb_Scheme.Entry{query}, true, "app", 0},
		{"unscoped, a query", []*Ydb_Scheme.Entry{query}, false, "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			snapshot := scopedSnapshot()
			inspect.ScopeStreamingInventory(snapshot, inventoryOf(c, test.entries...), test.scoped)
			c.Assert(snapshot.Coverage.Kinds, qt.HasLen, 1)
			c.Assert(snapshot.Coverage.Kinds[0].Regions, qt.HasLen, 1)
			c.Assert(snapshot.Coverage.Kinds[0].Regions[0].Scope.GetSchema().GetNormalized(), qt.Equals, test.wantSchema)
			c.Assert(snapshot.Coverage.Kinds[0].Regions[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
			c.Assert(snapshot.NativeObjects, qt.HasLen, test.wantValues)
		})
	}
}
