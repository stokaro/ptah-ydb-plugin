// Package topicplan lowers captured topic changes into the host's shared
// graph: one step per topic, refused where YDB cannot make the change in
// place or the target lacks a capability the topic needs.
package topicplan

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ServiceID names the topic planner.
const ServiceID = "ydb-topics"

// Plan contributes operations, footprints, dependencies, and a receipt per input.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	return objectplan.Plan(ctx, request, objectplan.Family{
		ServiceID: ServiceID, Kind: schema.TopicProperties, ChangeKind: schema.TopicChange,
		Capability: topics.CapabilityTopics, Noun: "topic", StepPrefix: "topic",
		Strategy: "Create, change or drop the topic and its consumers with CREATE TOPIC, ALTER TOPIC or DROP TOPIC",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.TopicProperties, subject)
		},
		NormalizeChange: schema.NormalizeTopicChange,
		Operation:       schema.TopicOperationFromChange,
		Refuse:          refuse,
		Present: func(change *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.TopicOperands(change)
			return before != nil, after != nil, err
		},
	})
}

// refuse says why a topic change cannot be planned on the target: the
// capabilities a declared topic needs, and a change YDB cannot make in place.
func refuse(change *protocol.Envelope, capabilities map[string]bool) error {
	current, desired, err := schema.TopicSpecs(change)
	if err != nil {
		return err
	}
	owner := change.GetOwner()
	if desired != nil {
		if err := topics.Check(owner.GetSchema(), owner.GetName(), *desired, capabilities); err != nil {
			return err
		}
	}
	if current != nil && desired != nil {
		if err := topics.ChangeRefusal(owner.GetSchema(), owner.GetName(), *desired, *current); err != nil {
			return err
		}
	}
	return nil
}
