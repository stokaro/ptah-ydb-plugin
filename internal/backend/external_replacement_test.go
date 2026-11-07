package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A data source given another type, with no table over it, is replaced in
// place where the target has CREATE OR REPLACE. A table moved off a data
// source the plan drops onto one it creates is dropped and created again
// around the drop: the new source first, then the table's drop, the old
// source's drop and the table's creation. Ported from the host's
// TestGenerateMigrationAST_External_Replacement.
func TestExternalPlanningRetypesAndMoves_HappyPath(t *testing.T) {
	bucket := external{name: "bucket", source: bucketAt("https://a/")}
	retyped := external{name: "bucket", source: &ydbdecl.ExternalDataSource{SourceType: "Ydb", Location: "grpc://ydb:2136", AuthMethod: "NONE",
		Options: map[string]string{"DATABASE_NAME": "/remote"}}}
	other := external{name: "other", source: bucketAt("https://a/")}
	events, moved := external{name: "events", table: eventsOver("app/bucket")}, external{name: "events", table: eventsOver("app/other")}
	for _, test := range []struct {
		name           string
		declared, read []external
		steps          []stepSummary
		strategies     []string
		edges          []string
	}{
		{name: "a source given another type, with no table over it", declared: []external{retyped}, read: []external{bucket},
			steps:      []stepSummary{{"bucket", schema.ExternalReplace, protocol.PlanningEffect_ALTER}},
			strategies: []string{"replace the data source in place with CREATE OR REPLACE, with the external tables over it dropped first"}},
		{name: "a table moved off a source the plan drops", declared: []external{other, moved}, read: []external{bucket, events},
			steps: []stepSummary{{"events", schema.ExternalDrop, protocol.PlanningEffect_DROP}, {"bucket", schema.ExternalDrop, protocol.PlanningEffect_DROP},
				{"other", schema.ExternalCreate, protocol.PlanningEffect_CREATE}, {"events", schema.ExternalRestore, protocol.PlanningEffect_CREATE}},
			strategies: []string{"create the data source", "drop the data source; the system it reads is untouched", "drop the external table and create it again"},
			edges:      []string{"events drop -> bucket drop", "events drop -> events create", "other create -> events create"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			planning, plan := externalPlan(c, test.declared, test.read, withReplace(), withReplace())

			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			var steps []stepSummary
			names := make(map[string]string)
			for _, step := range plan.Contributions[0].Steps {
				_, operation, err := schema.ExternalOperationChange(step.Operation.Value.Value)
				c.Assert(err, qt.IsNil)
				steps = append(steps, stepSummary{step.Operation.Value.Value.Owner.Name, operation, step.Metadata.Effects[0].Action})
				names[step.Id.Name] = step.Operation.Value.Value.Owner.Name + " " + map[protocol.PlanningEffect_Action]string{
					protocol.PlanningEffect_DROP: "drop", protocol.PlanningEffect_CREATE: "create", protocol.PlanningEffect_ALTER: "alter"}[step.Metadata.Effects[0].Action]
			}
			c.Assert(steps, qt.DeepEquals, test.steps)
			var strategies []string
			for _, receipt := range plan.Changes {
				strategies = append(strategies, receipt.Strategy)
			}
			c.Assert(strategies, qt.DeepEquals, test.strategies)
			var edges []string
			for _, edge := range plan.Contributions[0].Dependencies {
				edges = append(edges, names[edge.Before.Name]+" -> "+names[edge.After.Name])
			}
			c.Assert(edges, qt.DeepEquals, test.edges)
		})
	}
}
