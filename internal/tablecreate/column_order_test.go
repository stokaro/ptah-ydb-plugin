package tablecreate_test

import (
	"maps"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hostCycle joins the plan's dependencies with the order the host keeps, and
// returns the steps of a cycle the two close, or nil. It follows the host's
// scheduling of a planning reply: the host runs request.CommonSteps in the
// order it lists them; a step that replaces a host step takes that step's
// place in the order, and so does a dependency that names the host step; and
// any other step that is not a lifecycle step runs in the window of its phase,
// between the window's two markers. A step that drops something joins
// common/feature-removals, and any other step common/feature-creations.
func hostCycle(request *protocol.PlanFeaturesRequest, plan *protocol.PlannedFeatures) []string {
	replacement := make(map[string]string)
	for _, rewrite := range plan.Rewrites {
		for _, source := range rewrite.Sources {
			replacement[source.Name] = rewrite.Replacement.Name
		}
	}
	node := func(name string) string {
		if replaced, found := replacement[name]; found {
			return replaced
		}
		return name
	}
	later := make(map[string][]string)
	link := func(before, after string) {
		later[node(before)] = append(later[node(before)], node(after))
	}
	for n := 1; n < len(request.CommonSteps); n++ {
		link(request.CommonSteps[n-1].Id.Name, request.CommonSteps[n].Id.Name)
	}
	replacements := slices.Collect(func(yield func(string) bool) {
		for _, name := range replacement {
			if !yield(name) {
				return
			}
		}
	})
	for _, contribution := range plan.Contributions {
		for _, dependency := range contribution.Dependencies {
			link(dependency.Before.Name, dependency.After.Name)
		}
		for _, step := range contribution.Steps {
			if slices.Contains(replacements, step.Id.Name) || step.Operation.Phase == protocol.PlanningOperation_LIFECYCLE {
				continue
			}
			window := "feature-creations"
			if slices.ContainsFunc(step.Metadata.Effects, func(effect *protocol.PlanningEffect) bool { return effect.Action == protocol.PlanningEffect_DROP }) {
				window = "feature-removals"
			}
			link("common/"+window, step.Id.Name)
			link(step.Id.Name, "common/after-"+window)
		}
	}
	return cycle(later)
}

// cycle returns the steps of a cycle in the graph later describes, or nil.
func cycle(later map[string][]string) []string {
	const (
		unseen = iota
		open
		closed
	)
	state := make(map[string]int)
	var path []string
	var visit func(string) []string
	visit = func(name string) []string {
		switch state[name] {
		case open:
			return append(slices.Clone(path[slices.Index(path, name):]), name)
		case closed:
			return nil
		}
		state[name] = open
		path = append(path, name)
		for _, next := range later[name] {
			if found := visit(next); found != nil {
				return found
			}
		}
		path = path[:len(path)-1]
		state[name] = closed
		return nil
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range later {
			if !yield(name) {
				return
			}
		}
	})
	for _, name := range names {
		if found := visit(name); found != nil {
			return found
		}
	}
	return nil
}

// marker is one of the host's window markers, which belong to no table and
// change nothing.
func marker(name string) *protocol.CommonPlanningStep {
	return commonStep("common/"+name, nil, "")
}

// hostOrder lists a planning request's host steps the way the host orders an
// alteration of a table it keeps: the index removals, the column additions,
// the common index additions, the creation window, the removal window and the
// column removals.
func hostOrder(indexRemovals, additions, indexAdditions, removals []*protocol.CommonPlanningStep) []*protocol.CommonPlanningStep {
	return slices.Concat([]*protocol.CommonPlanningStep{marker("lifecycle-operations")}, indexRemovals, additions, indexAdditions,
		[]*protocol.CommonPlanningStep{marker("feature-creations"), marker("after-feature-creations"),
			marker("feature-removals"), marker("after-feature-removals")}, removals,
		[]*protocol.CommonPlanningStep{marker("after-lifecycle-operations")})
}

// inventoryOrderRequest alters app.records, which holds label and old, the
// index by_old over old and the index by_label over label, while the
// declaration states label and the INTEGER column new. YDB adds new with a
// column inventory change, since the host spells its type as an alias. label
// is Utf8 unless in.types gives it another type, on both sides.
func inventoryOrderRequest(c *qt.C, in alterCase) *protocol.PlanFeaturesRequest {
	c.Helper()
	held := map[string]string{"label": "Utf8", "old": "Utf8"}
	maps.Copy(held, in.types)
	in.types = map[string]string{"new": "INTEGER", "label": held["label"]}
	in.declared, in.held = []string{"label", "new"}, []string{"label", "old"}
	in.heldColumns = map[string]string{"label": "{}", "old": "{}"}
	in.heldIndexes = heldIndexes(indexJSON("globalIndex", "by_old", []string{"old"}, nil), indexJSON("globalIndex", "by_label", byLabel, nil))
	request := alterRequest(c, in)
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	for _, column := range request.Tables[0].Current.Columns {
		column.DataType, column.IsNullable = held[column.Name], "YES"
	}
	request.Tables[0].Current.Columns[0].DataType, request.Tables[0].Current.Columns[0].IsNullable = "Uint64", "NO"
	return request
}

func integer(column *protocol.PlanningColumn) { column.Type = "INTEGER" }

// The column changes of a table run in sequence, and the table's other steps
// keep the place the host gives them: a step that replaces a host step takes
// its place, and any other step runs in the window of its phase. The host
// drops an index before it adds a column and adds a common index before the
// creation window, so a sequence through every step of the table closes a
// cycle with the host's order, and the host refuses the plan.
func TestColumnOrder_KeepsTheHostsOrder(t *testing.T) {
	for _, test := range []struct {
		name     string
		in       alterCase
		declared []*protocol.DeclaredIndex
		steps    []*protocol.CommonPlanningStep
		planned  []string
		rewrites []string
		edges    []string
	}{
		{name: "drops an indexed column with its index and adds another",
			steps: hostOrder([]*protocol.CommonPlanningStep{commonIndexStep("drop-by-old", "by_old", protocol.PlanningEffect_DROP)},
				[]*protocol.CommonPlanningStep{additionStep("add-new", "new", integer)}, nil,
				[]*protocol.CommonPlanningStep{removalStep("drop-old", "old")}),
			planned: []string{"row-table-drop-column/000000 row-table-alter-operation", "column-inventory/000000 column-inventory-operation",
				"row-table-drop-index/000000 row-table-alter-operation"},
			rewrites: []string{"drop-old => row-table-drop-column/000000", "add-new => column-inventory/000000", "drop-by-old => row-table-drop-index/000000"},
			edges:    []string{"row-table-drop-index/000000 -> row-table-drop-column/000000", "column-inventory/000000 -> row-table-drop-column/000000"}},
		{name: "drops the index of a column it keeps and adds a column",
			steps: hostOrder([]*protocol.CommonPlanningStep{commonIndexStep("drop-by-label", "by_label", protocol.PlanningEffect_DROP)},
				[]*protocol.CommonPlanningStep{additionStep("add-new", "new", integer)}, nil,
				[]*protocol.CommonPlanningStep{removalStep("drop-old", "old")}),
			planned: []string{"row-table-drop-column/000000 row-table-alter-operation", "column-inventory/000000 column-inventory-operation",
				"row-table-drop-index/000000 row-table-alter-operation"},
			rewrites: []string{"drop-old => row-table-drop-column/000000", "add-new => column-inventory/000000", "drop-by-label => row-table-drop-index/000000"},
			edges:    []string{"column-inventory/000000 -> row-table-drop-column/000000"}},
		{name: "adds a common index and changes the TTL",
			in: alterCase{desiredTable: ttlTable(`{"dateTypeColumn":{"columnName":"label","expireAfterSeconds":3600}}`), heldTable: plainTable,
				types: map[string]string{"label": "Timestamp"}},
			declared: []*protocol.DeclaredIndex{{Name: "by_new", Fields: []string{"new"}}},
			steps: hostOrder(nil, []*protocol.CommonPlanningStep{additionStep("add-new", "new", integer)},
				[]*protocol.CommonPlanningStep{commonIndexStep("add-by-new", "by_new", protocol.PlanningEffect_CREATE)},
				[]*protocol.CommonPlanningStep{removalStep("drop-old", "old")}),
			planned: []string{"row-table-drop-column/000000 row-table-alter-operation", "column-inventory/000000 column-inventory-operation",
				"row-table-ttl/000000 row-table-alter-operation", "row-table-add-index/000000 row-table-alter-operation"},
			rewrites: []string{"drop-old => row-table-drop-column/000000", "add-new => column-inventory/000000", "add-by-new => row-table-add-index/000000"},
			edges:    []string{"add-new -> row-table-add-index/000000", "column-inventory/000000 -> row-table-drop-column/000000"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := inventoryOrderRequest(c, test.in)
			request.Tables[0].Desired.Indexes = test.declared
			request.CommonSteps = test.steps
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plannedSteps(plan), qt.DeepEquals, test.planned)
			c.Assert(rewrites(plan), qt.DeepEquals, test.rewrites)
			c.Assert(hostCycle(request, plan), qt.IsNil)
			c.Assert(edges(plan), qt.DeepEquals, test.edges)
		})
	}
}
