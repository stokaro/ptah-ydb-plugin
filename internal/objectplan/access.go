package objectplan

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// accessContext separates common principal changes from scheme occupants.
// Native grants read their object and change a distinct access resource. The
// host's typed operand and both effects must agree before this distinction is
// allowed to remove a step from physical-path conflict detection.
func accessContext(common *protocol.CommonPlanningStep) (principal bool, object, access *protocol.ComparisonIdentity, revoke bool, err error) {
	change := common.GetCommonObject()
	switch change.GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateRole, *protocol.CommonObjectChange_AlterRole, *protocol.CommonObjectChange_DropRole,
		*protocol.CommonObjectChange_AddMember, *protocol.CommonObjectChange_RemoveMember:
		for _, effect := range common.GetMetadata().GetEffects() {
			if effect.GetSubject().GetKind() != "role" {
				return false, nil, nil, false, fmt.Errorf("principal step claims a scheme object effect")
			}
		}
		return true, nil, nil, false, nil
	}
	grant := change.GetGrant()
	if grant == nil {
		grant, revoke = change.GetRevoke(), true
	}
	if grant == nil {
		return false, nil, nil, false, nil
	}
	for _, effect := range common.GetMetadata().GetEffects() {
		if effect.GetSubject().GetKind() == "object_access" {
			if access != nil || effect.GetAction() != protocol.PlanningEffect_ALTER {
				return false, nil, nil, false, fmt.Errorf("access step has an invalid access effect")
			}
			access = effect.GetSubject()
		} else if effect.GetAction() == protocol.PlanningEffect_READ && (schema.IsNativeObject(effect.GetSubject().GetKind()) || schema.IsWorkloadObject(effect.GetSubject().GetKind())) {
			if object != nil {
				return false, nil, nil, false, fmt.Errorf("access step reads several native objects")
			}
			object = effect.GetSubject()
		}
	}
	if access == nil && object == nil {
		return false, nil, nil, false, nil
	}
	if access == nil || object == nil || len(common.GetMetadata().GetEffects()) != 2 {
		return false, nil, nil, false, fmt.Errorf("access step needs its native object and access resource")
	}
	expected := proto.CloneOf(object)
	expected.Kind = "object_access"
	expected.Path = []*protocol.ComparisonPart{{Source: object.Kind, Normalized: object.Kind}}
	target := grant.GetTarget()
	if !proto.Equal(expected, access) || target.GetEngine() != "ydb" || target.GetName() != object.GetName().GetSource() ||
		target.GetSchema() != object.GetSchema().GetSource() || target.GetCatalog() != "" || len(target.GetPath())+len(target.GetOverload()) != 0 {
		return false, nil, nil, false, fmt.Errorf("access operand differs from its structural footprint")
	}
	if target.GetDatabase() != "" && object.GetDatabase().GetSource() != "" && target.GetDatabase() != object.GetDatabase().GetSource() {
		return false, nil, nil, false, fmt.Errorf("access operand names another database")
	}
	return false, object, access, revoke, nil
}

// AccessOrdering carries the graph changes needed to preserve captured access.
type AccessOrdering struct {
	Dependencies []*protocol.PlanningDependency
	Effects      []*protocol.PlanningEffect
	Sources      []*protocol.StepIdentity
}

// PathContext retains physical occupants after validating and separating common access steps.
func PathContext(request *protocol.PlanFeaturesRequest) (*protocol.PlanFeaturesRequest, error) {
	result := proto.CloneOf(request)
	result.CommonSteps = nil
	for _, common := range request.GetCommonSteps() {
		principal, object, _, _, err := accessContext(common)
		if err != nil {
			return nil, err
		}
		if !principal && object == nil {
			result.CommonSteps = append(result.CommonSteps, proto.CloneOf(common))
		}
	}
	return result, nil
}

// OrderAccess keeps exact captured ACL preconditions ahead of authorized ACL
// edits. A drop accounts for a matching revoke by clearing the object's ACL;
// the generic graph still checks every effect of the replaced common step.
func OrderAccess(request *protocol.PlanFeaturesRequest, family Family, subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action, id *protocol.StepIdentity) (AccessOrdering, error) {
	var result AccessOrdering
	addEdge := func(after *protocol.StepIdentity) {
		result.Dependencies = append(result.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(after)})
	}
	var seen []*protocol.ComparisonIdentity
	for _, common := range request.GetCommonSteps() {
		principal, object, access, revoke, err := accessContext(common)
		if err != nil {
			return result, err
		}
		if principal {
			continue
		}
		if object == nil || !sameAccessSubject(object, subject) {
			continue
		}
		grant := common.GetCommonObject().GetGrant()
		if revoke {
			grant = common.GetCommonObject().GetRevoke()
		}
		if strings.Join(strings.Fields(strings.ToLower(grant.GetTarget().GetKind())), " ") != family.Noun {
			return result, fmt.Errorf("access operand kind differs from its native object")
		}
		bound := request.GetDatabasePath()
		if bound == "" {
			bound = subject.GetDatabase().GetSource()
		}
		if db := grant.GetTarget().GetDatabase(); db != "" && bound != "" && db != bound {
			return result, fmt.Errorf("access operand names another session database")
		}
		if action == protocol.PlanningEffect_DROP {
			if !revoke {
				return result, fmt.Errorf("a grant targets a native object the plan drops")
			}
			result.Sources = append(result.Sources, proto.CloneOf(common.Id))
		} else {
			if action == protocol.PlanningEffect_CREATE && revoke {
				return result, fmt.Errorf("a revoke targets a native object whose captured state is absent")
			}
			addEdge(common.Id)
		}
		duplicate := false
		for _, known := range seen {
			duplicate = duplicate || provider.CompareIdentities(known, access) == 0
		}
		if !duplicate {
			seen = append(seen, access)
			accessAction := protocol.PlanningEffect_READ
			if action == protocol.PlanningEffect_DROP {
				accessAction = protocol.PlanningEffect_ALTER
			}
			result.Effects = append(result.Effects, &protocol.PlanningEffect{Subject: proto.CloneOf(access), Action: accessAction})
		}
	}
	return result, nil
}

// Native scheme entries and workload catalog entries both carry access. Their
// identities stay distinct even when their visible names match.
func sameAccessSubject(a, b *protocol.ComparisonIdentity) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	a.Database, b.Database = &protocol.ComparisonPart{}, &protocol.ComparisonPart{}
	return provider.CompareIdentities(a, b) == 0
}

// BeforePrincipalChanges keeps an observed native operation ahead of principal
// edits that can change its captured access. A creation has no such capture.
// Callers that route by a newly declared principal supply their own ordering.
func BeforePrincipalChanges(request *protocol.PlanFeaturesRequest, action protocol.PlanningEffect_Action, id *protocol.StepIdentity) []*protocol.PlanningDependency {
	if action == protocol.PlanningEffect_CREATE {
		return nil
	}
	var result []*protocol.PlanningDependency
	for _, common := range request.GetCommonSteps() {
		principal, _, _, _, _ := accessContext(common)
		if principal {
			result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(common.Id)})
		}
	}
	return result
}
