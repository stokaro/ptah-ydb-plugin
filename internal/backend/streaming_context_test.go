package backend_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func streamingContextState(c *qt.C, representation protocol.ValueInput_Representation, value *protocol.Envelope) *protocol.ComparisonState {
	c.Helper()
	var model *protocol.ValueModel
	for _, candidate := range schema.Models() {
		if candidate.Kind == schema.StreamingQueryProperties && candidate.Representation == representation {
			model = candidate
		}
	}
	c.Assert(model, qt.IsNotNil)
	state := testcomparison.State(model)
	if value != nil {
		appendContextValue(c, state, value)
	}
	return state
}

func appendContextValue(c *qt.C, state *protocol.ComparisonState, value *protocol.Envelope) {
	c.Helper()
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Features, Values: []*protocol.ValueInput{{Id: value.Owner.Name, Representation: state.Representation, Value: value}}})
	c.Assert(err, qt.IsNil)
	state.Records = append(state.Records, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
}

func unchangedStreamingRequest(c *qt.C, run *bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"}
	spec := streaming.Spec{Text: "SELECT * FROM `app/input`;", Run: run}
	desired, err := schema.CaptureStreamingQueryIntent(owner, spec, false)
	c.Assert(err, qt.IsNil)
	current, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: streamingplan.ServiceID, DatabasePath: "/local", ContextKinds: []string{schema.StreamingQueryProperties}, TargetFeatures: backend.Description().Features, Capabilities: map[string]bool{streaming.Capability: true, "transactional_ddl": false}, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, DesiredObjects: streamingContextState(c, protocol.ValueInput_DESIRED, desired), CurrentObjects: streamingContextState(c, protocol.ValueInput_OBSERVED, current)}
	inputOwner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "input"}
	readOwner := proto.CloneOf(inputOwner)
	readOwner.Database = "/local"
	inputCurrent, err := schema.CaptureObject(readOwner, readTopic())
	c.Assert(err, qt.IsNil)
	inputDesired, err := schema.ConvertNative(inputCurrent, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	inputDesired.Owner = proto.CloneOf(inputOwner)
	appendContextValue(c, request.DesiredObjects, inputDesired)
	appendContextValue(c, request.CurrentObjects, inputCurrent)
	request.CurrentObjects.Records[len(request.CurrentObjects.Records)-1].Subject.Database = nil
	request.DesiredObjects.Records[len(request.DesiredObjects.Records)-1].Subject.Database = nil
	request.ChangedSubjects = []*protocol.ComparisonIdentity{proto.CloneOf(request.CurrentObjects.Records[len(request.CurrentObjects.Records)-1].Subject)}
	return request
}

func TestUnchangedStreamingQueryRestartsAroundChangedInput(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Objects, qt.HasLen, 1)
	c.Assert(result.Objects[0].Steps, qt.HasLen, 2)
	steps := result.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(steps[0].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_EARLY)
	c.Assert(steps[1].Metadata.Placement, qt.Equals, protocol.PlanningMetadata_DEFAULT)
	rendered := testplanning.RenderRequest(backend.Description(), result, request.Capabilities)
	rendered.Form = protocol.RenderRequest_TEXT
	rendered.TargetFeatures = []string{provider.TextUnitsFeature}
	reply, err := backend.New().Render(c.Context(), rendered)
	c.Assert(err, qt.IsNil)
	c.Assert(reply.Units[0].GetSql(), qt.Equals, "ALTER STREAMING QUERY `app/copy` SET (RUN = FALSE, RESOURCE_POOL = `default`);")
	c.Assert(reply.Units[1].GetSql(), qt.Equals, "ALTER STREAMING QUERY `app/copy` SET (RUN = TRUE, RESOURCE_POOL = `default`);")
}

func TestUnchangedStreamingQueryFixedPoints(t *testing.T) {
	for _, test := range []struct {
		name   string
		run    *bool
		change bool
	}{{"unchanged input", nil, false}, {"stopped query", new(false), true}} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := unchangedStreamingRequest(c, test.run)
			if !test.change {
				request.ChangedSubjects = nil
			}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Objects, qt.HasLen, 1)
			c.Assert(result.Objects[0].Steps, qt.HasLen, 0)
		})
	}
}

func TestUnchangedStreamingQueryRefusesMissingInput(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	request.DesiredObjects.Records = request.DesiredObjects.Records[:1]
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Objects, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetContextObject(), qt.Equals, uint32(0))
	c.Assert(result.Diagnostics[0].Message, qt.Contains, "captured surviving")
}

func TestStreamingCaptureRequiresUnchangedObjectAssessment(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	value := request.DesiredObjects.Records[0].Value.Value
	c.Assert(value.RequiredFeatures, qt.Contains, provider.PlanningObjectContextFeature)
	reply, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "query", Representation: protocol.ValueInput_DESIRED, Value: value}}})
	c.Assert(err, qt.IsNotNil)
	c.Assert(reply, qt.IsNil)
}

func TestUnchangedStreamingQueryRefusesIncompleteObservation(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	request.CurrentObjects.Records = request.CurrentObjects.Records[1:]
	request.CurrentObjects.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Contains, "complete streaming query inventory")
}

func TestUnchangedStreamingQueryDoesNotUseAnotherDatabaseInput(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	foreign := request.DesiredObjects.Records[1]
	foreign.Subject.Database = testcomparison.Part("/other")
	foreign.Value.Value.Owner.Database = "/other"
	foreign.Value.NormalizedOwner.Database = "/other"
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Contains, "captured surviving")
}

func replaceContextQuery(c *qt.C, request *protocol.PlanFeaturesRequest, spec streaming.Spec, bound *protocol.DatabaseContext) {
	c.Helper()
	desired, err := schema.CaptureStreamingQueryIntent(request.DesiredObjects.Records[0].Value.Value.Owner, spec, false)
	c.Assert(err, qt.IsNil)
	desired.Context = proto.CloneOf(bound)
	current, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	for _, side := range []struct {
		state *protocol.ComparisonState
		value *protocol.Envelope
	}{{request.DesiredObjects, desired}, {request.CurrentObjects, current}} {
		previous := side.state.Records[1:]
		side.state.Records = nil
		appendContextValue(c, side.state, side.value)
		side.state.Records = append(side.state.Records, previous...)
	}
}

func TestUnchangedStreamingRestartPreservesCapturedContext(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	bound := &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
	replaceContextQuery(c, request, streaming.Spec{Text: "SELECT * FROM `/local/app/input`;"}, bound)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Contributions, qt.HasLen, 1)
	for _, step := range result.Contributions[0].Steps {
		c.Assert(step.Operation.Value.Value.Context, qt.CmpEquals(protocmp.Transform()), bound)
		before, after, err := schema.StreamingQueryOperands(schema.StreamingQueryOperationChange(step.Operation.Value.Value))
		c.Assert(err, qt.IsNil)
		c.Assert(before.Context, qt.CmpEquals(protocmp.Transform()), bound)
		c.Assert(after.Context, qt.CmpEquals(protocmp.Transform()), bound)
	}
}

func TestUnchangedStreamingQueryUsesDatabaseWidePoolIdentity(t *testing.T) {
	for _, survives := range []bool{true, false} {
		t.Run(map[bool]string{true: "surviving pool", false: "removed pool"}[survives], func(t *testing.T) {
			c := testcheck.New(t)
			request := unchangedStreamingRequest(c, nil)
			replaceContextQuery(c, request, streaming.Spec{Text: "SELECT * FROM `app/input`;", ResourcePool: "team.batch"}, nil)
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: "team.batch"}
			desired, err := schema.CaptureWorkloadIntent(owner, &native.ResourcePoolSettings{})
			c.Assert(err, qt.IsNil)
			current, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			appendContextValue(c, request.CurrentObjects, current)
			request.ChangedSubjects = []*protocol.ComparisonIdentity{proto.CloneOf(request.CurrentObjects.Records[2].Subject)}
			if survives {
				appendContextValue(c, request.DesiredObjects, desired)
			}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			if !survives {
				c.Assert(result.Contributions, qt.HasLen, 0)
				c.Assert(result.Diagnostics, qt.HasLen, 1)
				c.Assert(result.Diagnostics[0].Message, qt.Contains, "resource pool")
				return
			}
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Contributions[0].Steps, qt.HasLen, 2)
			for _, step := range result.Contributions[0].Steps {
				var pools []string
				for _, effect := range step.Metadata.Effects {
					if effect.Subject.Kind == schema.ResourcePoolProperties {
						pools = append(pools, effect.Subject.GetName().GetNormalized())
						c.Assert(effect.Subject.GetSchema().GetNormalized(), qt.Equals, "")
					}
				}
				c.Assert(pools, qt.DeepEquals, []string{"team.batch"})
			}
		})
	}
}

func TestUnchangedStreamingQueryOrdersCommonInputReplacement(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	request.ChangedSubjects = nil
	table := &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part("app"), Name: testcomparison.Part("input")}
	common := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "drop-old-table"}, Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Replace input", Effects: []*protocol.PlanningEffect{{Subject: table, Action: protocol.PlanningEffect_DROP}}}}
	request.CommonSteps = []*protocol.CommonPlanningStep{common}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	steps := result.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(result.Contributions[0].Dependencies, qt.CmpEquals(protocmp.Transform()), []*protocol.PlanningDependency{{Before: steps[0].Id, After: common.Id}, {Before: common.Id, After: steps[1].Id}, {Before: steps[0].Id, After: steps[1].Id}})
	for _, step := range steps {
		c.Assert(slices.ContainsFunc(step.Metadata.Effects, func(effect *protocol.PlanningEffect) bool {
			return effect.Action == protocol.PlanningEffect_READ && provider.CompareIdentities(effect.Subject, objectplan.Slot(table)) == 0
		}), qt.IsTrue)
	}
}

func TestUnchangedStreamingRestartRetainsAccessOrdering(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	access := commonAccessFor(request.DesiredObjects.Records[0].Subject, "streaming query", revokeAccess)
	request.CommonSteps = []*protocol.CommonPlanningStep{access}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Rewrites, qt.HasLen, 0)
	for _, step := range result.Contributions[0].Steps {
		c.Assert(slices.ContainsFunc(result.Contributions[0].Dependencies, func(edge *protocol.PlanningDependency) bool {
			return proto.Equal(edge.Before, step.Id) && proto.Equal(edge.After, access.Id)
		}), qt.IsTrue)
		c.Assert(slices.ContainsFunc(step.Metadata.Effects, func(effect *protocol.PlanningEffect) bool {
			return effect.Subject.Kind == "object_access" && effect.Action == protocol.PlanningEffect_READ
		}), qt.IsTrue)
	}
}

func TestStreamingContextRequiresCompleteUnnamedInventoryForWrites(t *testing.T) {
	for _, test := range []struct {
		name             string
		writes, complete bool
	}{{"unknown inventory with writes", true, false}, {"unknown inventory fixed point", false, false}, {"complete selected database", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := unchangedStreamingRequest(c, nil)
			request.CurrentObjects.Records = request.CurrentObjects.Records[1:]
			request.DesiredObjects.Records = request.DesiredObjects.Records[1:]
			source := request.CurrentObjects.Coverage.Kinds[0].Sources[0]
			source.Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
			if test.complete {
				source.Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Database: testcomparison.Part("/local"), Parent: &protocol.ComparisonPart{}, SubjectKinds: []string{schema.StreamingQueryProperties}}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
			}
			if !test.writes {
				request.ChangedSubjects = nil
			}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Contributions, qt.HasLen, 0)
			if test.writes && !test.complete {
				c.Assert(result.Diagnostics, qt.HasLen, 1)
				c.Assert(result.Diagnostics[0].Message, qt.Contains, "complete streaming query inventory")
				return
			}
			c.Assert(result.Diagnostics, qt.HasLen, 0)
		})
	}
}

// Creating an object needs no inventory of the streaming queries, since no
// query reads an object the database does not hold yet. Dropping one does.
func TestStreamingContextCreationNeedsNoInventory(t *testing.T) {
	for _, test := range []struct {
		name        string
		action      protocol.PlanningEffect_Action
		diagnostics int
	}{{"a table creation", protocol.PlanningEffect_CREATE, 0}, {"a table removal", protocol.PlanningEffect_DROP, 1}} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := unchangedStreamingRequest(c, nil)
			request.CurrentObjects.Records = request.CurrentObjects.Records[1:]
			request.DesiredObjects.Records = request.DesiredObjects.Records[1:]
			request.CurrentObjects.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
			request.ChangedSubjects = nil
			table := &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part("app"), Name: testcomparison.Part("fresh")}
			request.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "change-table"},
				Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Change a table", Effects: []*protocol.PlanningEffect{{Subject: table, Action: test.action}}}}}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, test.diagnostics)
		})
	}
}

// contextInventoryRequest is a filtered plan that writes the input app/input:
// its own coverage knows only the scope, and its context inventory covers the
// whole database and lists the queries outside the scope, here outside.
func contextInventoryRequest(c *qt.C, outside ...*protocol.Envelope) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := unchangedStreamingRequest(c, nil)
	request.CurrentObjects.Records = request.CurrentObjects.Records[1:]
	request.DesiredObjects.Records = request.DesiredObjects.Records[1:]
	request.CurrentObjects.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	request.ContextInventory = streamingContextState(c, protocol.ValueInput_OBSERVED, nil)
	source := request.ContextInventory.Coverage.Kinds[0].Sources[0]
	source.Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	source.Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Database: testcomparison.Part("/local"), Parent: &protocol.ComparisonPart{}, SubjectKinds: []string{schema.StreamingQueryProperties}}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
	for _, value := range outside {
		appendContextValue(c, request.ContextInventory, value)
	}
	return request
}

func outsideQuery(c *qt.C, text string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "other", Name: "outside"}
	desired, err := schema.CaptureStreamingQueryIntent(owner, streaming.Spec{Text: text}, false)
	c.Assert(err, qt.IsNil)
	observed, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	return observed
}

// A filtered plan may write an input when its context inventory proves that
// no streaming query outside the scope reads it.
func TestStreamingContextInventoryProvesTheOutside_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		outside []*protocol.Envelope
	}{
		{"no query outside the scope", nil},
		{"a query outside the scope that reads another input", []*protocol.Envelope{outsideQuery(testcheck.New(t), "SELECT * FROM `other/source`;")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().PlanFeatures(c.Context(), contextInventoryRequest(c, test.outside...))
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
		})
	}
}

// A running query outside the scope that reads the changed input refuses the
// plan by name, and an unread query outside it refuses as unknown.
func TestStreamingContextInventoryProvesTheOutside_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := contextInventoryRequest(c, outsideQuery(c, "SELECT * FROM `app/input`;"))
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Contains, `streaming query "other/outside" outside the command's scope reads a changed input; include its directory in the scope so the plan can stop and restart it`)
	request = contextInventoryRequest(c)
	unknown := &protocol.ComparisonIdentity{Kind: schema.StreamingQueryProperties, Schema: testcomparison.Part("other"), Name: testcomparison.Part("unread")}
	request.ContextInventory.Coverage.Subjects = append(request.ContextInventory.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: unknown.Kind, Subject: unknown,
		Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)})
	result, err = backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Contains, "cannot exclude unknown streaming queries")
}

// reservedContextName is a read's claim, on the streaming query kind, for
// name at the database root: no object of the kind is representable there,
// for reason.
func reservedContextName(name, reason string) *protocol.ComparisonSubjectCoverage {
	return &protocol.ComparisonSubjectCoverage{Kind: schema.StreamingQueryProperties,
		Subject:   &protocol.ComparisonIdentity{Kind: schema.StreamingQueryProperties, Name: testcomparison.Part(name)},
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: reason}}
}

// contextSteps is the number of steps each object receipt of result holds, by
// the object's name.
func contextSteps(result *protocol.PlannedFeatures) map[string]int {
	steps := make(map[string]int, len(result.Objects))
	for _, receipt := range result.Objects {
		steps[receipt.Subject.GetName().GetSource()] = len(receipt.Steps)
	}
	return steps
}

// A read claims the name of the provider's own storage, the lock node at the
// root, on every native kind. The claim is no streaming query, so a plan that
// changes an input restarts the query that reads it and asks nothing of the
// reserved name, which keeps a receipt without steps.
func TestUnchangedStreamingQueryBesideAReservedName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	request.CurrentObjects.Coverage.Subjects = append(request.CurrentObjects.Coverage.Subjects, reservedContextName(locking.NodeName, schema.ReservedNameReason))
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(contextSteps(result), qt.DeepEquals, map[string]int{"copy": 2, locking.NodeName: 0})
}

// A name the read could not represent for another reason may hold a query
// that reads the changed input, so the plan is still refused.
func TestUnchangedStreamingQueryBesideAReservedName_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := unchangedStreamingRequest(c, nil)
	request.CurrentObjects.Coverage.Subjects = append(request.CurrentObjects.Coverage.Subjects, reservedContextName("unread", "The query uses a setting the provider does not model"))
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Contributions, qt.HasLen, 0)
	c.Assert(result.Objects, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Equals, `streaming query "unread" has no complete captured definition: The query uses a setting the provider does not model`)
}
