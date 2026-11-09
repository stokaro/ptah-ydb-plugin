package backend

import (
	"slices"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// rebindRenamedIndexChanges gives a reversed index-settings change the name
// the reverse plan uses. The host reverses a rename's common step and moves
// the change's subject to the old name, but keeps the forward name in the
// captured value's owner. Rebinding is valid only when that same request has
// the matching rename step and its full read, drop and create footprint.
func rebindRenamedIndexChanges(request *protocol.PlanFeaturesRequest) *protocol.PlanFeaturesRequest {
	if request.GetPlanningId() != tablecreate.ServiceID || !slices.Contains(request.GetTargetFeatures(), provider.IndexRenamesFeature) {
		return request
	}
	var rebound *protocol.PlanFeaturesRequest
	for i, change := range request.GetChanges() {
		subject, value := change.GetSubject(), change.GetValue().GetValue()
		owner := value.GetOwner()
		if subject.GetKind() != "index" || value.GetKind() != schema.TablePropertiesChange || owner.GetKind() != "index" ||
			owner.GetName() == subject.GetName().GetSource() || !renamedIndexChangeBound(request, subject, owner) {
			continue
		}
		// The host's captured change must be valid before its owner is changed.
		// Re-normalizing a forged receipt here would otherwise hide the forgery
		// from the provider's request validation.
		verified, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: change.GetValue().GetId(), Representation: protocol.ValueInput_CHANGE, Value: value})
		if err != nil || !proto.Equal(verified, change.GetValue()) {
			continue
		}
		bound := proto.CloneOf(value)
		bound.Owner.Name = subject.GetName().GetSource()
		normalized, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: change.GetValue().GetId(), Representation: protocol.ValueInput_CHANGE, Value: bound})
		if err != nil {
			continue
		}
		if rebound == nil {
			rebound = proto.CloneOf(request)
		}
		rebound.Changes[i].Value = normalized
	}
	if rebound == nil {
		return request
	}
	return rebound
}

func renamedIndexChangeBound(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity, owner *protocol.Reference) bool {
	if owner.GetEngine() != request.GetEngine() || owner.GetSchema() != subject.GetSchema().GetSource() ||
		!slices.Equal(owner.GetPath(), []string{subject.GetParent().GetSource()}) || subject.GetName().GetSource() == "" {
		return false
	}
	table := proto.CloneOf(subject)
	table.Kind, table.Name, table.Parent = "table", proto.CloneOf(subject.GetParent()), nil
	old := proto.CloneOf(subject)
	old.Name.Source, old.Name.Normalized = owner.GetName(), owner.GetName()
	for _, step := range request.GetCommonSteps() {
		rename, effects := step.GetRenamedIndex(), step.GetMetadata().GetEffects()
		if rename.GetFrom() != owner.GetName() || rename.GetTo() != subject.GetName().GetSource() ||
			step.GetParent() == nil || provider.CompareIdentities(step.GetParent(), table) != 0 || len(effects) != 3 {
			continue
		}
		if effects[0].GetSubject() == nil || effects[1].GetSubject() == nil || effects[2].GetSubject() == nil {
			continue
		}
		if effects[0].GetAction() == protocol.PlanningEffect_READ && provider.CompareIdentities(effects[0].GetSubject(), table) == 0 &&
			effects[1].GetAction() == protocol.PlanningEffect_DROP && provider.CompareIdentities(effects[1].GetSubject(), old) == 0 &&
			effects[2].GetAction() == protocol.PlanningEffect_CREATE && provider.CompareIdentities(effects[2].GetSubject(), subject) == 0 {
			return true
		}
	}
	return false
}
