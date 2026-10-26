package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func streamingPlanRequest(c *qt.C, before, after *streaming.Spec, allow bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"}
	var observed, desired *protocol.Envelope
	var err error
	if before != nil {
		observed, err = schema.CaptureObject(owner, &native.StreamingQuerySettings{Text: before.Text, Run: before.Run, ResourcePool: before.ResourcePool})
		c.Assert(err, qt.IsNil)
	}
	if after != nil {
		desired, err = schema.CaptureStreamingQueryIntent(owner, *after, allow)
		c.Assert(err, qt.IsNil)
	}
	change, err := schema.CaptureStreamingQueryChange("change", owner, observed, desired)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: streamingplan.ServiceID,
		Capabilities: map[string]bool{streaming.Capability: true, "transactional_ddl": false},
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Changes:      []*protocol.PlanningChange{{Subject: testplanning.Subject(owner), Value: change}}}
}

func TestStreamingPlanningSplitsRunningChanges(t *testing.T) {
	body := "INSERT INTO `app/sink` SELECT * FROM `app/source`;"
	for _, test := range []struct {
		name          string
		before, after *streaming.Spec
		count         int
		early         bool
	}{
		{"create", nil, &streaming.Spec{Text: body}, 1, false},
		{"drop", &streaming.Spec{Text: body}, nil, 1, true},
		{"stop only", &streaming.Spec{Text: body}, &streaming.Spec{Text: body, Run: new(false)}, 1, true},
		{"change running body", &streaming.Spec{Text: body}, &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/next`;"}, 2, true},
		{"change stopped body", &streaming.Spec{Text: body, Run: new(false)}, &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/next`;"}, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := streamingPlanRequest(c, test.before, test.after, true)
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			steps := result.Contributions[0].Steps
			c.Assert(steps, qt.HasLen, test.count)
			c.Assert(steps[0].Metadata.Placement == protocol.PlanningMetadata_EARLY, qt.Equals, test.early)
			rendering := testplanning.RenderRequest(result, request.Capabilities)
			rendering.Form, rendering.TargetFeatures = protocol.RenderRequest_TEXT, []string{provider.TextUnitsFeature}
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, test.count)
			if test.count == 2 {
				c.Assert(rendered.Units[0].GetSql(), qt.Equals, "ALTER STREAMING QUERY `app/copy` SET (RUN = FALSE, RESOURCE_POOL = `default`);")
				c.Assert(rendered.Units[1].GetSql(), qt.Contains, "FORCE = TRUE")
				stopped, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(steps[1].Operation.Value.Value))
				c.Assert(err, qt.IsNil)
				c.Assert(streaming.Running(*stopped.Before), qt.IsFalse)
				c.Assert(steps[1].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_DEFAULT)
			}
		})
	}
}

func TestStreamingPlanningRequiresCapability(t *testing.T) {
	c := qt.New(t)
	request := streamingPlanRequest(c, nil, &streaming.Spec{Text: "SELECT 1;"}, false)
	request.Capabilities[streaming.Capability] = false
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
}
