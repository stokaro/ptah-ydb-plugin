package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func commonNativeAccess(request *protocol.PlanFeaturesRequest, revoke bool) *protocol.CommonPlanningStep {
	return commonAccessFor(request.Changes[0].Subject, "coordination node", revoke)
}

func commonAccessFor(subject *protocol.ComparisonIdentity, kind string, revoke bool) *protocol.CommonPlanningStep {
	subject = proto.CloneOf(subject)
	access := proto.CloneOf(subject)
	access.Kind = "object_access"
	access.Path = []*protocol.ComparisonPart{{Source: subject.Kind, Normalized: subject.Kind}}
	grant := &protocol.SQLGrant{Role: "app", Privileges: []string{"SELECT"}, Target: &protocol.Reference{Engine: "ydb", Kind: kind,
		Database: subject.GetDatabase().GetSource(), Schema: subject.GetSchema().GetSource(), Name: subject.GetName().GetSource()}}
	change := &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: grant}}
	if revoke {
		change.Operation = &protocol.CommonObjectChange_Revoke{Revoke: grant}
	}
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "access"}, CommonObject: change,
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Change native access",
			Effects: []*protocol.PlanningEffect{{Subject: subject, Action: protocol.PlanningEffect_READ}, {Subject: access, Action: protocol.PlanningEffect_ALTER}}}}
}

func TestNativeAccessOrderingPreservesCapturedACLChecks(t *testing.T) {
	for _, action := range []string{"create", "alter", "drop"} {
		t.Run(action, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, action)
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			request.CommonSteps = []*protocol.CommonPlanningStep{commonNativeAccess(request, action == "drop")}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			step := result.Contributions[0].Steps[0]
			c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
			c.Assert(step.Metadata.Effects, qt.HasLen, 3)
			if action == "drop" {
				c.Assert(result.Rewrites, qt.HasLen, 1)
				c.Assert(proto.Equal(result.Rewrites[0].Sources[0], request.CommonSteps[0].Id), qt.IsTrue)
				c.Assert(step.Metadata.Effects[2].Action, qt.Equals, protocol.PlanningEffect_ALTER)
				return
			}
			c.Assert(result.Rewrites, qt.HasLen, 0)
			c.Assert(result.Contributions[0].Dependencies, qt.HasLen, 1)
			c.Assert(proto.Equal(result.Contributions[0].Dependencies[0].Before, step.Id), qt.IsTrue)
			c.Assert(proto.Equal(result.Contributions[0].Dependencies[0].After, request.CommonSteps[0].Id), qt.IsTrue)
			c.Assert(step.Metadata.Effects[2].Action, qt.Equals, protocol.PlanningEffect_READ)
		})
	}
}

func TestNativeAccessOrderingRefusesContradictoryOrForgedContext(t *testing.T) {
	for _, test := range []struct {
		name, action string
		revoke       bool
		change       func(*protocol.PlanFeaturesRequest)
	}{
		{name: "grant dropped object", action: "drop"},
		{name: "revoke absent object", action: "create", revoke: true},
		{name: "unnegotiated lifecycle", action: "alter", change: func(r *protocol.PlanFeaturesRequest) { r.TargetFeatures = []string{provider.CommonObjectsFeature} }},
		{name: "foreign database", action: "alter", change: func(r *protocol.PlanFeaturesRequest) {
			r.CommonSteps[0].CommonObject.GetGrant().Target.Database = "/foreign"
		}},
		{name: "different operand", action: "alter", change: func(r *protocol.PlanFeaturesRequest) { r.CommonSteps[0].CommonObject.GetGrant().Target.Name = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, test.action)
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			request.CommonSteps = []*protocol.CommonPlanningStep{commonNativeAccess(request, test.revoke)}
			if test.change != nil {
				test.change(request)
			}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Rewrites, qt.HasLen, 0)
		})
	}
}

func TestExternalRecreationKeepsRevokesAfterRestoration(t *testing.T) {
	c := qt.New(t)
	bucket := external{name: "bucket", source: bucketAt("https://a/")}
	moved := external{name: "bucket", source: bucketAt("https://b/")}
	events := external{name: "events", table: eventsOver("app/bucket")}
	request, _ := externalPlan(c, []external{moved, events}, []external{bucket, events}, withSources(), withSources())
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
	for _, change := range request.Changes {
		if change.Subject.Kind == schema.ExternalTableProperties {
			request.CommonSteps = []*protocol.CommonPlanningStep{commonAccessFor(change.Subject, "external table", true)}
		}
	}
	c.Assert(request.CommonSteps, qt.HasLen, 1)
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Rewrites, qt.HasLen, 0)
	var guarded int
	for _, step := range result.Contributions[0].Steps {
		if step.Operation.Value.Value.Owner.Kind != schema.ExternalTableProperties {
			continue
		}
		guarded++
		c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
		var ordered bool
		for _, edge := range result.Contributions[0].Dependencies {
			ordered = ordered || proto.Equal(edge.Before, step.Id) && proto.Equal(edge.After, request.CommonSteps[0].Id)
		}
		c.Assert(ordered, qt.IsTrue)
		var readsAccess bool
		for _, effect := range step.Metadata.Effects {
			readsAccess = readsAccess || effect.Subject.Kind == "object_access" && effect.Action == protocol.PlanningEffect_READ
		}
		c.Assert(readsAccess, qt.IsTrue)
	}
	c.Assert(guarded, qt.Equals, 2)
}

func TestWorkloadDropRewritesRevokesAsOneCompleteReplacement(t *testing.T) {
	c := qt.New(t)
	request, _ := workloadPlan(c, workloadCapabilities(), nil, workloadRequest(c, schema.ResourcePoolProperties, nil, nil))
	for _, name := range []string{"batch", "online"} {
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Database: "/local", Name: name}
		before, err := schema.CaptureWorkloadObservation(owner, pool(workload.PoolSpec{}))
		c.Assert(err, qt.IsNil)
		change, err := schema.CaptureResourcePoolChange(name, owner, before, nil)
		c.Assert(err, qt.IsNil)
		request.Changes = append(request.Changes, &protocol.PlanningChange{Subject: workloadSubject(schema.ResourcePoolProperties, name, "/local"), Value: change})
	}
	c.Assert(request.Changes, qt.HasLen, 2)
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
	for _, change := range request.Changes {
		common := commonAccessFor(change.Subject, "resource pool", true)
		common.Id.Name += "/" + change.Subject.Name.Source
		request.CommonSteps = append(request.CommonSteps, common)
	}
	result, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Rewrites, qt.HasLen, 1)
	c.Assert(result.Rewrites[0].Sources, qt.HasLen, 2)
	c.Assert(result.Contributions[0].Steps, qt.HasLen, 1)
	step := result.Contributions[0].Steps[0]
	c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
	c.Assert(step.Metadata.Effects, qt.HasLen, 4)
	var accessCount int
	for _, effect := range step.Metadata.Effects {
		if effect.Subject.Kind == "object_access" {
			accessCount++
			c.Assert(effect.Action, qt.Equals, protocol.PlanningEffect_ALTER)
		}
	}
	c.Assert(accessCount, qt.Equals, 2)
}

func TestNativeAccessOrderingWithPrincipalEdits(t *testing.T) {
	for _, action := range []string{"create", "alter"} {
		t.Run(action, func(t *testing.T) {
			c := qt.New(t)
			request := nativePlan(c, action)
			request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature}
			common := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "principal"}, Metadata: &protocol.PlanningMetadata{
				Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: "Change a principal",
				Effects: []*protocol.PlanningEffect{{Subject: &protocol.ComparisonIdentity{Kind: "role", Name: &protocol.ComparisonPart{Source: "app", Normalized: "app"}}, Action: protocol.PlanningEffect_ALTER}}}}
			if action == "create" {
				common.CommonObject = &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: "app"}}}}
				common.Metadata.Effects[0].Action = protocol.PlanningEffect_CREATE
			} else {
				common.CommonObject = &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_RemoveMember{RemoveMember: &protocol.RoleMembership{Group: "team", Member: "app"}}}
			}
			request.CommonSteps = []*protocol.CommonPlanningStep{commonNativeAccess(request, action == "alter"), common}
			result, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
			var beforePrincipal bool
			for _, edge := range result.Contributions[0].Dependencies {
				beforePrincipal = beforePrincipal || proto.Equal(edge.After, common.Id)
			}
			c.Assert(beforePrincipal, qt.Equals, action == "alter")
		})
	}
}
