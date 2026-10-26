package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

func TestStreamingSourcePreservesPoolThroughConversion(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"copy":{"schema":"app","text":"SELECT 1;","resource_pool":"team.batch"}}}`)
	c.Assert(err, qt.IsNil)
	document := request.Documents[0]
	document.Models = []*protocol.ValueModel{schema.NativeDesiredModel(schema.StreamingQueryProperties)}
	document.Fragments[0].YamlKey = streamingsource.YAMLKey()
	document.Fragments[0].Selector = streamingsource.YAMLKey().Selector
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := decoded.Documents[0].Fragments[0].Values[0].Value
	c.Assert(value.RequiredObjects, qt.HasLen, 1)
	converted, err := backend.New().ConvertValues(c.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_DESIRED,
		To: protocol.ValueInput_OBSERVED, Values: []*protocol.NormalizedValue{value}})
	c.Assert(err, qt.IsNil)
	c.Assert(converted.Values[0].RequiredObjects, qt.CmpEquals(protocmp.Transform()), value.RequiredObjects)
}

func TestStreamingResourcesBindPathsAndPool(t *testing.T) {
	c := qt.New(t)
	// Absolute and relative aliases of the same path contribute one read.
	request := streamingPlanRequest(c, nil, &streaming.Spec{Text: "PRAGMA TablePathPrefix = '/local/app'; INSERT INTO sink SELECT * FROM source JOIN `/local/app/source` ON true;", ResourcePool: "team.batch"}, false)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	effects := result.Contributions[0].Steps[0].Metadata.Effects
	var paths []string
	var pools []string
	for _, effect := range effects {
		if effect.Action != protocol.PlanningEffect_READ {
			continue
		}
		c.Assert(effect.Subject.GetDatabase().GetNormalized(), qt.Equals, "")
		switch effect.Subject.Kind {
		case "ydb.scheme-path":
			paths = append(paths, effect.Subject.GetSchema().GetNormalized()+"/"+effect.Subject.GetName().GetNormalized())
		case schema.ResourcePoolProperties:
			c.Assert(effect.Subject.GetSchema().GetNormalized(), qt.Equals, "")
			pools = append(pools, effect.Subject.GetName().GetNormalized())
		default:
			c.Fatalf("invented a resource kind: %s", effect.Subject.Kind)
		}
	}
	c.Assert(paths, qt.DeepEquals, []string{"app/sink", "app/source"})
	c.Assert(pools, qt.DeepEquals, []string{"team.batch"})
}

func TestStreamingResourcesRefuseForeignAndUnresolvedPaths(t *testing.T) {
	for _, text := range []string{
		"SELECT * FROM `/other/app/source`;",
		"SELECT * FROM `/local-other/app/source`;",
		"SELECT * FROM `/local/app/source`; PRAGMA TablePathPrefix = $prefix; SELECT * FROM source;",
		"PRAGMA TablePathPrefix = '../app'; SELECT * FROM source;",
	} {
		c := qt.New(t)
		request := streamingPlanRequest(c, nil, &streaming.Spec{Text: text}, false)
		result, err := backend.New().PlanFeatures(c.Context(), request)
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
	}
	c := qt.New(t)
	request := streamingPlanRequest(c, nil, &streaming.Spec{Text: "SELECT * FROM `/local/outside/source`;"}, false)
	request.DatabasePath = "/local/dev"
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestStreamingResourcesOrderStopsAndRestartsAroundCommonInputs(t *testing.T) {
	c := qt.New(t)
	before := &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `/local/app/input`;"}
	after := &streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/input` WHERE true;"}
	request := streamingPlanRequest(c, before, after, true)
	table := &protocol.ComparisonIdentity{Kind: "table", Schema: &protocol.ComparisonPart{Source: "app", Normalized: "app"}, Name: &protocol.ComparisonPart{Source: "input", Normalized: "input"}}
	common := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "replace-input"}, Metadata: &protocol.PlanningMetadata{
		Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: "Change input table", Effects: []*protocol.PlanningEffect{{Subject: table, Action: protocol.PlanningEffect_ALTER}}}}
	request.CommonSteps = []*protocol.CommonPlanningStep{common}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	steps := result.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(result.Contributions[0].Dependencies, qt.CmpEquals(protocmp.Transform()), []*protocol.PlanningDependency{
		{Before: steps[0].Id, After: common.Id},
		{Before: common.Id, After: steps[1].Id},
		{Before: steps[0].Id, After: steps[1].Id},
	})
}

func TestStreamingSplitPreservesAccessOrderingAndDropRewrite(t *testing.T) {
	for _, action := range []string{"alter", "drop"} {
		t.Run(action, func(t *testing.T) {
			c := qt.New(t)
			before := &streaming.Spec{Text: "SELECT * FROM `app/source`;"}
			after := &streaming.Spec{Text: "SELECT * FROM `app/next`;"}
			if action == "drop" {
				after = nil
			}
			request := streamingPlanRequest(c, before, after, true)
			request.TargetFeatures = backend.Description().Engines[0].Features
			access := commonAccessFor(request.Changes[0].Subject, "streaming query", true)
			request.CommonSteps = []*protocol.CommonPlanningStep{access}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			steps := result.Contributions[0].Steps
			if action == "drop" {
				c.Assert(steps, qt.HasLen, 1)
				c.Assert(result.Rewrites, qt.HasLen, 1)
				c.Assert(proto.Equal(result.Rewrites[0].Replacement, steps[0].Id), qt.IsTrue)
				c.Assert(result.Rewrites[0].Sources, qt.CmpEquals(protocmp.Transform()), []*protocol.StepIdentity{access.Id})
				c.Assert(steps[0].Metadata.Effects[2].Action, qt.Equals, protocol.PlanningEffect_ALTER)
				return
			}
			c.Assert(result.Rewrites, qt.HasLen, 0)
			c.Assert(steps, qt.HasLen, 2)
			for _, step := range steps {
				c.Assert(step.Metadata.Effects[2].Subject.Kind, qt.Equals, "object_access")
				c.Assert(step.Metadata.Effects[2].Action, qt.Equals, protocol.PlanningEffect_READ)
				var beforeAccess bool
				for _, edge := range result.Contributions[0].Dependencies {
					beforeAccess = beforeAccess || proto.Equal(edge.Before, step.Id) && proto.Equal(edge.After, access.Id)
				}
				c.Assert(beforeAccess, qt.IsTrue)
			}
		})
	}
}

func TestStreamingResourcePoolSurvivesNormalization(t *testing.T) {
	for _, pool := range []string{"", "default", "team.batch"} {
		t.Run(pool, func(t *testing.T) {
			c := qt.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: "/local", Schema: "app", Name: "copy"}
			value, err := schema.CaptureStreamingQueryIntent(owner, streaming.Spec{Text: "SELECT 1;", ResourcePool: pool}, true)
			c.Assert(err, qt.IsNil)
			result, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "query", Representation: protocol.ValueInput_DESIRED, Value: value}}})
			c.Assert(err, qt.IsNil)
			if pool == "" || pool == "default" {
				c.Assert(result.Values[0].RequiredObjects, qt.HasLen, 0)
				return
			}
			c.Assert(result.Values[0].RequiredObjects, qt.CmpEquals(protocmp.Transform()), []*protocol.ComparisonIdentity{{Kind: schema.ResourcePoolProperties,
				Database: &protocol.ComparisonPart{Source: "/local", Normalized: "/local"}, Name: &protocol.ComparisonPart{Source: pool, Normalized: pool}}})
		})
	}
}

// The streaming observation contains query settings only, so a principal
// mutation cannot invalidate its captured native precondition.
func TestStreamingSettingsDoNotFencePrincipalChanges(t *testing.T) {
	c := qt.New(t)
	request := streamingPlanRequest(c, &streaming.Spec{Text: "SELECT 1;"}, &streaming.Spec{Text: "SELECT 2;"}, true)
	request.TargetFeatures = backend.Description().Engines[0].Features
	principal := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "principal"},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: "Change membership",
			Effects: []*protocol.PlanningEffect{{Subject: &protocol.ComparisonIdentity{Kind: "role", Name: &protocol.ComparisonPart{Source: "team", Normalized: "team"}}, Action: protocol.PlanningEffect_ALTER}}},
		CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_RemoveMember{RemoveMember: &protocol.RoleMembership{Group: "team", Member: "app"}}}}
	request.CommonSteps = []*protocol.CommonPlanningStep{principal}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Contributions[0].Steps, qt.HasLen, 2)
	for _, edge := range result.Contributions[0].Dependencies {
		c.Assert(proto.Equal(edge.Before, principal.Id) || proto.Equal(edge.After, principal.Id), qt.IsFalse)
	}
}
