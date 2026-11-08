package objectplan

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// PathSubject is the scheme path of the session's database at directory
// schema and leaf name, as a step's effect names one.
func PathSubject(schema, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: PathKind, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: schema, Normalized: schema}, Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
}

// sidePaths returns the effects of the paths family.Paths names for a change,
// and the edges that order the step against each common step acting on one
// of them: a path the step creates after a common step drops it and before a
// common step creates a view whose query reads it, a path the step drops
// before a common step creates it, and a path the step reads after
// a common step creates or changes it, or, for a step that drops its own
// object, before a common step changes or drops what it read. Two steps that
// both create or both drop one path are refused, as is a common step that
// changes a path the step creates or drops.
func sidePaths(request *protocol.PlanFeaturesRequest, family Family, item change, id *protocol.StepIdentity) ([]*protocol.PlanningEffect, []*protocol.PlanningDependency, error) {
	paths, err := family.Paths(request, item.value.Value)
	if err != nil {
		return nil, nil, err
	}
	var effects []*protocol.PlanningEffect
	var edges edgeList
	for _, path := range paths {
		if path.Subject != nil {
			effects = append(effects, &protocol.PlanningEffect{Subject: proto.CloneOf(path.Subject), Action: path.Action})
			continue
		}
		subject := PathSubject(path.Schema, path.Name)
		if samePath(subject, item.input.Subject) {
			continue
		}
		effects = append(effects, &protocol.PlanningEffect{Subject: subject, Action: path.Action})
		if err := orderSidePath(request, family, item, id, path, subject, &edges); err != nil {
			return nil, nil, err
		}
	}
	return effects, edges, nil
}

// orderSidePath adds to edges the order of the step id against each common
// step of request with an effect at subject, the scheme path of path, as
// sidePaths describes. A common step that changes a path the step creates or
// drops is refused.
func orderSidePath(
	request *protocol.PlanFeaturesRequest,
	family Family,
	item change,
	id *protocol.StepIdentity,
	path PathEffect,
	subject *protocol.ComparisonIdentity,
	edges *edgeList,
) error {
	for _, common := range request.CommonSteps {
		if path.Action == protocol.PlanningEffect_CREATE && createsViewReading(request.GetDatabasePath(), common, subject) {
			edges.add(id, common.Id)
		}
		for _, effect := range common.GetMetadata().GetEffects() {
			if effect.GetSubject().GetParent().GetNormalized() != "" || !isSchemeSubject(effect.GetSubject()) || provider.CompareIdentities(slot(effect.Subject), subject) != 0 {
				continue
			}
			switch {
			case effect.Action == protocol.PlanningEffect_READ && path.Action == protocol.PlanningEffect_DROP:
				edges.add(common.Id, id)
			case effect.Action == protocol.PlanningEffect_READ && path.Action == protocol.PlanningEffect_CREATE:
				edges.add(id, common.Id)
			case effect.Action == protocol.PlanningEffect_READ:
				// Two reads of one path need no order.
			case path.Action == protocol.PlanningEffect_CREATE && effect.Action == protocol.PlanningEffect_DROP:
				edges.add(common.Id, id)
			case path.Action == protocol.PlanningEffect_DROP && effect.Action == protocol.PlanningEffect_CREATE:
				edges.add(id, common.Id)
			case path.Action == protocol.PlanningEffect_READ && item.action == protocol.PlanningEffect_DROP:
				edges.add(id, common.Id)
			case path.Action == protocol.PlanningEffect_READ:
				edges.add(common.Id, id)
			default:
				return fmt.Errorf("%s %s path %s that a common step also changes", family.Noun, actionWord(path.Action), displayPath(path.Schema, path.Name))
			}
		}
	}
	return nil
}

func actionWord(action protocol.PlanningEffect_Action) string {
	if action == protocol.PlanningEffect_DROP {
		return "drops"
	}
	return "creates"
}

func displayPath(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "/" + name
}
