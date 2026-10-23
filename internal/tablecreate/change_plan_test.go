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

// tableChange captures the change a facet comparison reports for app.records.
func tableChange(c *qt.C, desired, held string) *protocol.PlanningChange {
	c.Helper()
	after, before := facet(c, schema.TableProperties, "", desired), observedFacet(c, schema.TableProperties, "", held)
	value, err := schema.CaptureTablePropertiesChange("change", after.Value.Value.Owner, before.Value.Value, after.Value.Value)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: tableSubject(), Value: value}
}

func ttlOnlyRequest(c *qt.C) *protocol.PlanFeaturesRequest {
	c.Helper()
	in := ttlCase(ttlTable(hourly), plainTable, "ts")
	in.types = ttlTypes
	request := alterRequest(c, in)
	request.Changes = []*protocol.PlanningChange{tableChange(c, ttlTable(hourly), plainTable)}
	return request
}

func TestChangePlan_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := ttlOnlyRequest(c)
	original := proto.CloneOf(request)

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-ttl/000000")
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Kind, qt.Equals, schema.TablePropertiesChange)
	c.Assert(proto.Equal(plan.Changes[0].Subject, request.Changes[0].Subject), qt.IsTrue)
	c.Assert(plan.Changes[0].Steps, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Steps[0].Name, qt.Equals, "row-table-ttl/000000")
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

func TestChangePlan_ColumnChangeBelongsToItsTable(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{desiredColumns: map[string]string{"note": `{"family":"archive"}`}, declared: []string{"note"},
		desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, heldTable: plainTable, held: []string{"note"},
		heldColumns: map[string]string{"note": `{}`}})
	after, before := facet(c, schema.ColumnProperties, "note", `{"family":"archive"}`), observedFacet(c, schema.ColumnProperties, "note", `{}`)
	value, err := schema.CaptureTablePropertiesChange("change", after.Value.Value.Owner, before.Value.Value, after.Value.Value)
	c.Assert(err, qt.IsNil)
	request.Changes = []*protocol.PlanningChange{{Subject: after.Subject, Value: value}}

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Steps, qt.Not(qt.HasLen), 0)
}

func TestChangePlan_AlreadySatisfied(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, ttlCase(ttlTable(hourly), ttlTable(hourly), "ts"))
	request.Changes = []*protocol.PlanningChange{tableChange(c, ttlTable(hourly), ttlTable(hourly))}

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Changes[0].Steps, qt.HasLen, 0)
}

func TestChangePlan_FailurePath(t *testing.T) {
	t.Run("a change of a table the request does not capture", func(t *testing.T) {
		c := qt.New(t)
		request := ttlOnlyRequest(c)
		request.Tables = nil

		// The backend refuses this request at the SDK boundary, so the service is called directly.
		plan, err := tablecreate.Plan(c.Context(), request)

		c.Assert(err, qt.IsNil)
		c.Assert(plan.Diagnostics, qt.HasLen, 1)
		c.Assert(plan.Diagnostics[0].Kind, qt.Equals, schema.TablePropertiesChange)
		c.Assert(plan.Diagnostics[0].GetChange(), qt.Equals, uint32(0))
		c.Assert(plan.Changes, qt.HasLen, 0)
	})
	t.Run("a forged normalization receipt", func(t *testing.T) {
		c := qt.New(t)
		request := ttlOnlyRequest(c)
		request.Changes[0].Value.Id = "forged"
		request.Changes[0].Value.CanonicalProjection = []byte("{}")

		_, err := tablecreate.Plan(c.Context(), request)

		c.Assert(err, qt.ErrorMatches, "YDB table change has a forged normalization receipt")
	})
	t.Run("a change of another model", func(t *testing.T) {
		c := qt.New(t)
		request := ttlOnlyRequest(c)
		request.Changes[0].Value.Value.Kind = schema.CoordinationChange

		_, err := tablecreate.Plan(c.Context(), request)

		c.Assert(err, qt.ErrorMatches, "YDB table planning received an unowned change")
	})
	t.Run("a change on the wrong subject", func(t *testing.T) {
		c := qt.New(t)
		request := ttlOnlyRequest(c)
		request.Changes[0].Subject.Name = &protocol.ComparisonPart{Source: "other", Normalized: "other"}

		_, err := tablecreate.Plan(c.Context(), request)

		c.Assert(err, qt.ErrorMatches, "YDB table change has a different bound identity")
	})
}
