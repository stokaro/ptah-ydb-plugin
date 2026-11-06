package objectplan

import (
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// FollowTheCommonSteps orders each step of contribution that late names after
// the last common step of request, other than a marker, that has to come
// before it: the step runs against the schema the rest of the plan leaves.
// The access on a native object that has to follow one of those steps, such
// as a grant on a streaming query the step creates, follows it instead. The
// host orders such access by its object rather than by its place among the
// common steps, so the common steps after it do not wait for it, and the
// anchor can be one of them. A principal change after such access waits for
// it, as does any other common step that has to follow one of the steps
// (see needsContribution), so the anchor is the last common step ahead of the
// first of them. A common step that rewrites replaces with a step of
// contribution is no anchor, since the replacement takes its place.
//
// This is where the YDB engine built into the host at 74de0b65b ran the
// creation or change of a streaming query, and a grant on one: after every
// statement of the host, and the grant last (its objectLifecycle.changedLast).
// Measured with schema apply, migrations generate and ptah-compat migrate
// diff on 26.2.1.14: a query between two topics beside a group, a table, a
// third topic and grants on the table, the third topic and the query planned
// the topics, the group, the table, the grants on the table and the topic,
// the query and the grant on it.
func FollowTheCommonSteps(request *protocol.PlanFeaturesRequest, contribution *protocol.PlanningContribution, late []*protocol.StepIdentity, rewrites []*protocol.PlanningRewrite) error {
	named := func(ids []*protocol.StepIdentity, id *protocol.StepIdentity) bool {
		return slices.ContainsFunc(ids, func(have *protocol.StepIdentity) bool { return proto.Equal(have, id) })
	}
	steps := &protocol.PlanningContribution{Owner: contribution.GetOwner()}
	for _, step := range contribution.GetSteps() {
		if named(late, step.GetId()) {
			steps.Steps = append(steps.Steps, step)
		}
	}
	for _, edge := range contribution.GetDependencies() {
		if named(late, edge.GetBefore()) {
			steps.Dependencies = append(steps.Dependencies, edge)
		}
	}
	if len(steps.Steps) == 0 {
		return nil
	}
	var replaced []*protocol.StepIdentity
	for _, rewrite := range rewrites {
		replaced = append(replaced, rewrite.GetSources()...)
	}
	var anchor *protocol.StepIdentity
	followed := false
	for _, common := range request.GetCommonSteps() {
		access, err := accessContext(common)
		if err != nil {
			return err
		}
		needed := needsContribution(common, steps)
		if needed && access.object != nil {
			followed = true
			continue
		}
		if needed || (followed && access.principal) {
			break
		}
		if !Marker(common) && !named(replaced, common.GetId()) {
			anchor = common.GetId()
		}
	}
	if anchor == nil {
		return nil
	}
	for _, step := range steps.Steps {
		edge := &protocol.PlanningDependency{Before: proto.CloneOf(anchor), After: proto.CloneOf(step.GetId())}
		if !slices.ContainsFunc(contribution.Dependencies, func(have *protocol.PlanningDependency) bool { return proto.Equal(have, edge) }) {
			contribution.Dependencies = append(contribution.Dependencies, edge)
		}
	}
	return nil
}
