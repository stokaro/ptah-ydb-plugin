package objectplan

import (
	"slices"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// FollowLateInOrder places each step of contribution, whose steps run in the
// order they are listed, after the common steps that need neither the step
// nor a step after it: the step follows the last common step other than a
// marker ahead of the first common step that has to follow one of them (see
// needsContribution). A step a common step needs, or one that runs before a
// step a common step needs, runs right before that common step, and a step no
// common step needs runs after them all. A common step that rewrites replaces
// with a step of contribution is never an anchor, since the replacement takes
// its place and its edges.
//
// This is the order the YDB engine built into the host at 74de0b65b gave the
// workload statements it chained under the owner of its common steps, whose
// names sort after theirs: a statement ran only once the common sequence
// could go no further without it, or after the whole sequence. Measured with
// schema apply and migrations generate on 25.1.4.7 and 26.2.1.14: a pool, its
// classifier and a grant on the pool beside a group, a table, a topic and
// their grants planned the pool before the grants and the classifier after
// them, and the reverse dropped the table, the classifier, the pool and then
// the group.
//
// A step's anchor is never later than the anchor of a step after it, since
// the steps from the earlier one on include those from the later one, so
// following the anchors closes no cycle with the edges that chain the steps.
func FollowLateInOrder(request *protocol.PlanFeaturesRequest, contribution *protocol.PlanningContribution, rewrites []*protocol.PlanningRewrite) {
	var replaced []*protocol.StepIdentity
	for _, rewrite := range rewrites {
		replaced = append(replaced, rewrite.GetSources()...)
	}
	var edges []*protocol.PlanningDependency
	for i, step := range contribution.GetSteps() {
		rest := &protocol.PlanningContribution{Owner: contribution.GetOwner(), Steps: contribution.GetSteps()[i:]}
		for _, edge := range contribution.GetDependencies() {
			if slices.ContainsFunc(rest.Steps, func(later *protocol.PlanningStep) bool { return proto.Equal(later.GetId(), edge.GetBefore()) }) {
				rest.Dependencies = append(rest.Dependencies, edge)
			}
		}
		var anchor *protocol.StepIdentity
		for _, common := range request.GetCommonSteps() {
			if needsContribution(common, rest) {
				break
			}
			if !Marker(common) && !slices.ContainsFunc(replaced, func(id *protocol.StepIdentity) bool { return proto.Equal(id, common.GetId()) }) {
				anchor = common.GetId()
			}
		}
		if anchor != nil {
			edges = append(edges, &protocol.PlanningDependency{Before: proto.CloneOf(anchor), After: proto.CloneOf(step.GetId())})
		}
	}
	for _, edge := range edges {
		if !slices.ContainsFunc(contribution.Dependencies, func(have *protocol.PlanningDependency) bool { return proto.Equal(have, edge) }) {
			contribution.Dependencies = append(contribution.Dependencies, edge)
		}
	}
}
