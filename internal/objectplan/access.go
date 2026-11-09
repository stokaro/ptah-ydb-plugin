package objectplan

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// accessStep is what accessContext reads of a common step: a principal
// change, or the native object a grant or a revoke reads and the access
// resource it changes. The zero value is a step of neither.
type accessStep struct {
	principal      bool
	object, access *protocol.ComparisonIdentity
	revoke         bool
}

// accessContext separates common principal changes from scheme occupants.
// Native grants read their object and change a distinct access resource. The
// host's typed operand and both effects must agree before this distinction is
// allowed to remove a step from physical-path conflict detection.
func accessContext(common *protocol.CommonPlanningStep) (accessStep, error) {
	principal, err := principalStep(common)
	if err != nil {
		return accessStep{}, err
	}
	if principal {
		return accessStep{principal: true}, nil
	}
	change := common.GetCommonObject()
	grant, revoke := change.GetGrant(), false
	if grant == nil {
		grant, revoke = change.GetRevoke(), true
	}
	if grant == nil {
		return accessStep{}, nil
	}
	object, access, err := accessEffects(common)
	if err != nil {
		return accessStep{}, err
	}
	if access == nil && object == nil {
		return accessStep{}, nil
	}
	if access == nil || object == nil || len(common.GetMetadata().GetEffects()) != 2 {
		return accessStep{}, fmt.Errorf("access step needs its native object and access resource")
	}
	if err := checkAccessOperand(grant.GetTarget(), object, access); err != nil {
		return accessStep{}, err
	}
	return accessStep{object: object, access: access, revoke: revoke}, nil
}

// principalStep reports whether common changes a principal: it creates,
// alters or drops a role, or adds or removes a member. Every effect of such a
// step must name a role.
func principalStep(common *protocol.CommonPlanningStep) (bool, error) {
	switch common.GetCommonObject().GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateRole, *protocol.CommonObjectChange_AlterRole, *protocol.CommonObjectChange_DropRole,
		*protocol.CommonObjectChange_AddMember, *protocol.CommonObjectChange_RemoveMember:
		for _, effect := range common.GetMetadata().GetEffects() {
			if effect.GetSubject().GetKind() != "role" {
				return false, fmt.Errorf("principal step claims a scheme object effect")
			}
		}
		return true, nil
	}
	return false, nil
}

// accessEffects returns the access resource a grant or a revoke step alters
// and the native or workload object it reads, either nil when the step has
// none. A second access effect, an access effect other than an alteration and
// a second object read are refused.
func accessEffects(common *protocol.CommonPlanningStep) (object, access *protocol.ComparisonIdentity, err error) {
	for _, effect := range common.GetMetadata().GetEffects() {
		if effect.GetSubject().GetKind() == "object_access" {
			if access != nil || effect.GetAction() != protocol.PlanningEffect_ALTER {
				return nil, nil, fmt.Errorf("access step has an invalid access effect")
			}
			access = effect.GetSubject()
		} else if effect.GetAction() == protocol.PlanningEffect_READ &&
			(schema.IsNativeObject(effect.GetSubject().GetKind()) || schema.IsWorkloadObject(effect.GetSubject().GetKind())) {
			if object != nil {
				return nil, nil, fmt.Errorf("access step reads several native objects")
			}
			object = effect.GetSubject()
		}
	}
	return object, access, nil
}

// checkAccessOperand refuses a grant's target that is not the object its step
// reads, or an access resource that is not that object's: the target must
// name the object's YDB name and schema with no catalog, path or overload,
// and a database both name must be the same.
func checkAccessOperand(target *protocol.Reference, object, access *protocol.ComparisonIdentity) error {
	expected := proto.CloneOf(object)
	expected.Kind = "object_access"
	expected.Path = []*protocol.ComparisonPart{{Source: object.Kind, Normalized: object.Kind}}
	if !proto.Equal(expected, access) || target.GetEngine() != "ydb" || target.GetName() != object.GetName().GetSource() ||
		target.GetSchema() != object.GetSchema().GetSource() || target.GetCatalog() != "" || len(target.GetPath())+len(target.GetOverload()) != 0 {
		return fmt.Errorf("access operand differs from its structural footprint")
	}
	if target.GetDatabase() != "" && object.GetDatabase().GetSource() != "" && target.GetDatabase() != object.GetDatabase().GetSource() {
		return fmt.Errorf("access operand names another database")
	}
	return nil
}

// AccessOrdering carries the graph changes needed to preserve captured access.
type AccessOrdering struct {
	Dependencies []*protocol.PlanningDependency
	Effects      []*protocol.PlanningEffect
	Sources      []*protocol.StepIdentity
}

// PathContext retains physical occupants after validating and separating common access steps.
func PathContext(request *protocol.PlanFeaturesRequest) (*protocol.PlanFeaturesRequest, error) {
	result := provider.CloneSharingModels(request)
	result.CommonSteps = nil
	for _, common := range request.GetCommonSteps() {
		step, err := accessContext(common)
		if err != nil {
			return nil, err
		}
		_, directory, err := directoryAccess(common)
		if err != nil {
			return nil, err
		}
		if !step.principal && step.object == nil && !directory {
			result.CommonSteps = append(result.CommonSteps, proto.CloneOf(common))
		}
	}
	return result, nil
}

// directoryAccess reports a common step that grants or revokes a permission
// on a directory, and the directory it names. Such a step changes the
// directory's access list: it occupies no path and creates or drops none, so
// it takes no part in path conflict detection. The host's typed operand and
// its one effect must agree before the step is set aside.
func directoryAccess(common *protocol.CommonPlanningStep) (string, bool, error) {
	change := common.GetCommonObject()
	grant := change.GetGrant()
	if grant == nil {
		grant = change.GetRevoke()
	}
	if grant == nil || strings.ToLower(strings.TrimSpace(grant.GetTarget().GetKind())) != "schema" {
		return "", false, nil
	}
	effects := common.GetMetadata().GetEffects()
	if len(effects) != 1 {
		return "", false, fmt.Errorf("directory access step needs exactly its directory's access list")
	}
	subject := effects[0].GetSubject()
	target := grant.GetTarget()
	if effects[0].GetAction() != protocol.PlanningEffect_ALTER || subject.GetKind() != "schema" || subject.GetName().GetSource() != target.GetName() ||
		subject.GetSchema().GetNormalized() != "" || subject.GetParent().GetNormalized() != "" || len(subject.GetPath())+len(subject.GetSignature()) != 0 ||
		target.GetSchema() != "" || target.GetCatalog() != "" || len(target.GetPath())+len(target.GetOverload()) != 0 || target.GetName() == "" {
		return "", false, fmt.Errorf("directory access operand differs from its structural footprint")
	}
	return target.GetName(), true, nil
}

// AfterCreationBelowDirectoryAccess keeps a native creation ahead of a grant
// or a revoke on a directory that holds it. Creating the object creates the
// directories above it that do not exist yet, and YDB grants nothing on a
// path that holds nothing. An operation other than a creation needs no order
// against a directory's access.
func AfterCreationBelowDirectoryAccess(request *protocol.PlanFeaturesRequest, action protocol.PlanningEffect_Action, subject *protocol.ComparisonIdentity, id *protocol.StepIdentity) ([]*protocol.PlanningDependency, error) {
	if action != protocol.PlanningEffect_CREATE {
		return nil, nil
	}
	var result []*protocol.PlanningDependency
	for _, common := range request.GetCommonSteps() {
		directory, granted, err := directoryAccess(common)
		if err != nil {
			return nil, err
		}
		holder := subject.GetSchema().GetSource()
		if granted && (holder == directory || strings.HasPrefix(holder, directory+"/")) {
			result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(common.Id)})
		}
	}
	return result, nil
}

// OrderAccess keeps exact captured ACL preconditions ahead of authorized ACL
// edits. A drop accounts for a matching revoke by clearing the object's ACL;
// the generic graph still checks every effect of the replaced common step.
func OrderAccess(request *protocol.PlanFeaturesRequest, family Family, subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action, id *protocol.StepIdentity) (AccessOrdering, error) {
	var result AccessOrdering
	var seen []*protocol.ComparisonIdentity
	for _, common := range request.GetCommonSteps() {
		step, err := accessContext(common)
		if err != nil {
			return result, err
		}
		if step.principal {
			continue
		}
		if step.object == nil || !sameAccessSubject(step.object, subject) {
			continue
		}
		grant := common.GetCommonObject().GetGrant()
		if step.revoke {
			grant = common.GetCommonObject().GetRevoke()
		}
		if err = checkAccessTarget(request, family, subject, grant); err != nil {
			return result, err
		}
		if action == protocol.PlanningEffect_DROP {
			if !step.revoke {
				return result, fmt.Errorf("a grant targets a native object the plan drops")
			}
			result.Sources = append(result.Sources, proto.CloneOf(common.Id))
		} else {
			if action == protocol.PlanningEffect_CREATE && step.revoke {
				return result, fmt.Errorf("a revoke targets a native object whose captured state is absent")
			}
			result.Dependencies = append(result.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(common.Id)})
		}
		if !slices.ContainsFunc(seen, func(known *protocol.ComparisonIdentity) bool {
			return provider.CompareIdentities(known, step.access) == 0
		}) {
			seen = append(seen, step.access)
			result.Effects = append(result.Effects, &protocol.PlanningEffect{Subject: proto.CloneOf(step.access), Action: accessEffectAction(action)})
		}
	}
	return result, nil
}

// checkAccessTarget refuses an access operand whose target kind, with its
// words lowered and single-spaced, is not the family's noun, and one that
// names a database other than the session's: the request's database path, or
// the subject's database when the request names none.
func checkAccessTarget(request *protocol.PlanFeaturesRequest, family Family, subject *protocol.ComparisonIdentity, grant *protocol.SQLGrant) error {
	if strings.Join(strings.Fields(strings.ToLower(grant.GetTarget().GetKind())), " ") != family.Noun {
		return fmt.Errorf("access operand kind differs from its native object")
	}
	bound := request.GetDatabasePath()
	if bound == "" {
		bound = subject.GetDatabase().GetSource()
	}
	if db := grant.GetTarget().GetDatabase(); db != "" && bound != "" && db != bound {
		return fmt.Errorf("access operand names another session database")
	}
	return nil
}

// accessEffectAction is what an operation of action does to the object's
// access resource: a drop alters it by clearing the object's ACL, and any
// other operation reads it.
func accessEffectAction(action protocol.PlanningEffect_Action) protocol.PlanningEffect_Action {
	if action == protocol.PlanningEffect_DROP {
		return protocol.PlanningEffect_ALTER
	}
	return protocol.PlanningEffect_READ
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
		if step, _ := accessContext(common); step.principal {
			result = append(result, &protocol.PlanningDependency{Before: proto.CloneOf(id), After: proto.CloneOf(common.Id)})
		}
	}
	return result
}
