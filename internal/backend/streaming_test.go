package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func streamingPlanRequest(c *qt.C, before, after *streaming.Spec, allow bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	return streamingPlanRequestAt(c, "app", "copy", before, after, allow)
}

// streamingPlanRequestAt is streamingPlanRequest for the query at
// directory/name.
func streamingPlanRequestAt(c *qt.C, directory, name string, before, after *streaming.Spec, allow bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: directory, Name: name}
	var observed, desired *protocol.Envelope
	var err error
	if before != nil {
		readOwner := proto.CloneOf(owner)
		readOwner.Database = "/local"
		observed, err = schema.CaptureObject(readOwner, &native.StreamingQuerySettings{Text: before.Text, Run: before.Run, ResourcePool: before.ResourcePool})
		c.Assert(err, qt.IsNil)
		observed.Owner = proto.CloneOf(owner)
	}
	if after != nil {
		desired, err = schema.CaptureStreamingQueryIntent(owner, *after, allow)
		c.Assert(err, qt.IsNil)
	}
	change, err := schema.CaptureStreamingQueryChange("change", owner, observed, desired)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature, provider.PlanningObjectContextFeature}, ContextKinds: []string{schema.StreamingQueryProperties}, DesiredObjects: streamingContextState(c, protocol.ValueInput_DESIRED, desired), CurrentObjects: streamingContextState(c, protocol.ValueInput_OBSERVED, observed), PlanningId: streamingplan.ServiceID, DatabasePath: "/local",
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
			c := testcheck.New(t)
			request := streamingPlanRequest(c, test.before, test.after, true)
			request.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "common/dependent-operations"}, Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Ordering marker"}}}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			steps := result.Contributions[0].Steps
			c.Assert(steps, qt.HasLen, test.count)
			for _, step := range steps {
				c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
			}
			c.Assert(result.Contributions[0].Dependencies, qt.HasLen, test.count-1)
			c.Assert(steps[0].Metadata.Placement == protocol.PlanningMetadata_EARLY, qt.Equals, test.early)
			rendering := testplanning.RenderRequest(backend.Description(), result, request.Capabilities)
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
	c := testcheck.New(t)
	request := streamingPlanRequest(c, nil, &streaming.Spec{Text: "SELECT 1;"}, false)
	request.Capabilities[streaming.Capability] = false
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
}

// One refused change refuses the whole batch: a creation at a path where a
// common step creates a table, beside a valid creation, plans nothing, and the
// refusal names the refused change's input. Ported from the host's
// TestStreamingInvalidChangeDiscardsTheWholeBatch. The host's invalid change,
// a running query's body changed without allow_state_reset, never reaches
// planning here: the change's capture refuses it.
func TestStreamingInvalidChangeDiscardsTheWholeBatch_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := streamingPlanRequestAt(c, "app", "added", nil, &streaming.Spec{Text: "SELECT 1;"}, false)
	clash := streamingPlanRequestAt(c, "app", "clash", nil, &streaming.Spec{Text: "SELECT 2;"}, false)
	clash.Changes[0].Value.Id = "clash"
	request.Changes = append(request.Changes, clash.Changes...)
	request.DesiredObjects.Records = append(request.DesiredObjects.Records, clash.DesiredObjects.Records...)
	request.CommonSteps = []*protocol.CommonPlanningStep{commonAt("table", "app", "clash", protocol.PlanningEffect_CREATE)}

	result, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetChange(), qt.Equals, uint32(1))
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
}

// A running query whose body changes is stopped early, before a user the plan
// creates and one it drops, and started again after both, as the YDB engine
// built into the host at 74de0b65b ran it: the stop is placed early rather
// than fenced against the principal changes, and the restart follows the last
// common step. Ported from the host's
// TestWorkloadStreamingStopsAroundPoolAndPrincipalChanges.
func TestStreamingStopsAroundPrincipalChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := streamingPlanRequest(c, &streaming.Spec{Text: "SELECT 1;"}, &streaming.Spec{Text: "SELECT 2;"}, true)
	request.TargetFeatures = backend.Description().Engines[0].Features
	role := func(name string) *protocol.ComparisonIdentity {
		return &protocol.ComparisonIdentity{Kind: "role", Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
	}
	request.CommonSteps = []*protocol.CommonPlanningStep{
		{Id: &protocol.StepIdentity{Owner: "host", Name: "create-user"}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN,
			Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Create a user", Effects: []*protocol.PlanningEffect{{Subject: role("new.user"), Action: protocol.PlanningEffect_CREATE}}},
			CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "new.user", Login: true}}}}},
		{Id: &protocol.StepIdentity{Owner: "host", Name: "drop-user"}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN,
			Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Drop a user", Effects: []*protocol.PlanningEffect{{Subject: role("old.user"), Action: protocol.PlanningEffect_DROP}}},
			CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropRole{DropRole: &protocol.Reference{Engine: "ydb", Kind: "role", Name: "old.user"}}}},
	}

	result, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	steps := result.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	stopped, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(steps[0].Operation.Value.Value))
	c.Assert(err, qt.IsNil)
	c.Assert(streaming.Running(*stopped.After), qt.IsFalse)
	c.Assert(steps[0].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_EARLY)
	c.Assert(steps[1].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_DEFAULT)
	c.Assert(stepEdges(result), qt.DeepEquals, []string{steps[0].Id.Name + " -> " + steps[1].Id.Name, "drop-user -> " + steps[1].Id.Name})
	c.Assert(result.Changes[0].Steps, qt.HasLen, 2)
}
