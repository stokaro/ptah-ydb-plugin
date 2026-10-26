package streamingplan

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

type lifecycleBuilder struct {
	request, paths *protocol.PlanFeaturesRequest
	contribution   *protocol.PlanningContribution
	rewrites       []*protocol.PlanningRewrite
}

func (b *lifecycleBuilder) add(prefix string, subject *protocol.ComparisonIdentity, owner *protocol.Reference, bound *protocol.DatabaseContext, parts []streaming.Change) ([]*protocol.StepIdentity, error) {
	var steps []*protocol.StepIdentity
	for ordinal, part := range parts {
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%d", prefix, ordinal)}
		value, err := operation(id.Name, owner, bound, part)
		if err != nil {
			return nil, err
		}
		action := protocol.PlanningEffect_ALTER
		if part.Before == nil {
			action = protocol.PlanningEffect_CREATE
		} else if part.After == nil {
			action = protocol.PlanningEffect_DROP
		}
		edges, err := objectplan.PathDependencies(b.paths, family(), subject, action, id)
		if err != nil {
			return nil, err
		}
		b.contribution.Dependencies = append(b.contribution.Dependencies, edges...)
		metadata := &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: value.OperationSemantics.Impact, Reason: value.OperationSemantics.Reason,
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: action}, {Subject: objectplan.Slot(subject), Action: action}}}
		access, err := objectplan.OrderAccess(b.request, family(), subject, action, id)
		if err != nil {
			return nil, err
		}
		metadata.Effects = append(metadata.Effects, access.Effects...)
		b.contribution.Dependencies = append(b.contribution.Dependencies, access.Dependencies...)
		// Streaming preconditions contain only the persistent query settings.
		// Principal edits do not change that capture; matching access writes
		// still follow the query operation through OrderAccess.
		if len(access.Sources) != 0 {
			b.rewrites = append(b.rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
		}
		spec := part.After
		if streaming.Early(part) {
			metadata.Placement = protocol.PlanningMetadata_EARLY
			spec = part.Before
		}
		reads, dependencies, err := queryResources(b.paths, subject, *spec, streaming.Early(part), id)
		if err != nil {
			return nil, err
		}
		metadata.Effects = append(metadata.Effects, reads...)
		b.contribution.Dependencies = append(b.contribution.Dependencies, dependencies...)
		if len(steps) > 0 {
			b.contribution.Dependencies = append(b.contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(steps[len(steps)-1]), After: proto.CloneOf(id)})
		}
		b.contribution.Steps = append(b.contribution.Steps, &protocol.PlanningStep{Id: id, Metadata: metadata, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value, Phase: protocol.PlanningOperation_LIFECYCLE}})
		steps = append(steps, proto.CloneOf(id))
	}
	return steps, nil
}
