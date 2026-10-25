package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// A coordination node operation renders in the TEXT form as the statement the
// host's embedded engine writes for it, which Execute runs through the
// coordination service: a CREATE with the declared settings, an ALTER with
// the ones that change, a DROP.
func TestCoordinationRendersText(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   string
	}{
		{action: "create", want: "CREATE COORDINATION NODE `app/node` WITH (read_consistency_mode = 'strict');"},
		{action: "alter", want: "ALTER COORDINATION NODE `app/node` SET (read_consistency_mode = 'strict');"},
		{action: "drop", want: "DROP COORDINATION NODE `app/node`;"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, tc.action)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			rendering := testplanning.RenderRequest(plan, request.Capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(rendered.Units[0].GetSql(), qt.Equals, tc.want)
			c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
		})
	}
}

// The TEXT form checks the batch as the native form does: a forged
// operation fails the call, and no unit is written.
func TestTextRenderRefusesAForgedOperation(t *testing.T) {
	c := qt.New(t)
	request := nativePlan(c, "create")
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendering.Changes[0].GetNative().Value.OperationSemantics.Access = protocol.ChangeSemantics_WIDENS
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.ErrorMatches, `.*native operation metadata changed`)
	c.Assert(rendered, qt.IsNil)
}

// The provider and its engine both list render.text-units, so a host that
// lists it negotiates the TEXT form.
func TestDescriptionOffersTextUnits(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	c.Assert(description.Features, qt.Contains, provider.TextUnitsFeature)
	c.Assert(description.Engines[0].Features, qt.Contains, provider.TextUnitsFeature)
	without := provider.NegotiatedDescription(description, nil)
	c.Assert(without.Engines[0].Features, qt.Not(qt.Contains), provider.TextUnitsFeature)
	with := provider.NegotiatedDescription(description, []string{provider.TextUnitsFeature})
	c.Assert(with.Engines[0].Features, qt.Contains, provider.TextUnitsFeature)
}
