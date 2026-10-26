package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func inventoryRequest(c *qt.C) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"new"}, held: []string{"old"}, heldColumns: map[string]string{"old": "{}"}})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	for _, column := range request.Tables[0].Current.Columns {
		column.DataType = "Utf8"
		column.IsNullable = "YES"
		if column.Name == "id" {
			column.DataType = "Uint64"
			column.IsNullable = "NO"
		}
	}
	return request
}

func TestColumnInventoryPlansFromTheCapturedParent(t *testing.T) {
	c := qt.New(t)
	request := inventoryRequest(c)
	original := proto.CloneOf(request)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Rewrites, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	steps := plan.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(steps[0].Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_CREATE)
	c.Assert(steps[1].Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_DROP)
	c.Assert(steps[1].Metadata.Impact, qt.Equals, protocol.ChangeSemantics_DESTRUCTIVE)
	add, err := tablecreate.DecodeInventory(steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(add.Add.Name, qt.Equals, "new")
	drop, err := tablecreate.DecodeInventory(steps[1].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(drop.Drop.Name, qt.Equals, "old")
	c.Assert(edges(plan), qt.DeepEquals, []string{"column-inventory/000000 -> column-inventory/000001"})
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 2)
	}
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

func TestColumnInventoryRefusesMissingStateAndUnsupportedAddition(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.PlanFeaturesRequest)
	}{
		{"missing native removal", func(r *protocol.PlanFeaturesRequest) {
			records := r.Tables[0].Current.Features.Facets.Records
			for i, record := range records {
				if record.Kind == schema.ColumnProperties {
					r.Tables[0].Current.Features.Facets.Records = append(records[:i], records[i+1:]...)
					break
				}
			}
		}},
		{"unknown nullability", func(r *protocol.PlanFeaturesRequest) { r.Tables[0].Current.Columns[1].IsNullable = "" }},
		{"not null addition", func(r *protocol.PlanFeaturesRequest) { r.Tables[0].Desired.Columns[1].Nullable = false }},
		{"default addition", func(r *protocol.PlanFeaturesRequest) { r.Tables[0].Desired.Columns[1].DefaultSet = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := inventoryRequest(c)
			test.mutate(request)
			plan, err := tablecreate.Plan(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestColumnInventoryRewritesTheRealAliasStep(t *testing.T) {
	c := qt.New(t)
	request := inventoryRequest(c)
	request.Tables[0].Desired.Columns[1].Type = "INTEGER"
	request.CommonSteps = []*protocol.CommonPlanningStep{additionStep("add-new", "new", func(column *protocol.PlanningColumn) { column.Type = "INTEGER" })}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(rewrites(plan), qt.DeepEquals, []string{"add-new => column-inventory/000000"})
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 2)
	add, err := tablecreate.DecodeInventory(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(add.Add.Type.GetOptionalType().GetItem().GetTypeId().String(), qt.Equals, "INT32")
}
