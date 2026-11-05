// Package topiccompare compares the declared and the read standalone YDB
// topics offline, as Ptah's embedded YDB engine compares them: each setting
// resolved to the value YDB gives a new topic, and the consumers by name.
package topiccompare

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the owner of topic comparisons.
const ServiceID = "ydb-topics"

// Compare evaluates complete operands and coverage without database access.
// A topic holding a setting no declaration states refuses the comparison, as
// the host refuses to read one.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, Family())
}

// Family is the topic's part of a comparison.
func Family() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.TopicProperties, Capability: topics.CapabilityTopics, Noun: "topic", DottedLimits: true,
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.TopicProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.TopicProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: schema.ValidateTopicAccess,
		Adopt:     adopt,
		Equal:     equal,
		Change:    schema.CaptureTopicChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			observed, err := schema.ObservedTopic(current)
			if err != nil {
				return nil, err
			}
			intent, err := topics.Keep(observed)
			if err != nil {
				return nil, err
			}
			return schema.CaptureTopicIntent(owner, intent)
		},
	}
}

// adopt gives the declared topic the consumers a transfer of the read reads
// it through, which the declaration cannot name; see
// [topics.AdoptTransferConsumers].
func adopt(current, desired *protocol.Envelope) (*protocol.Envelope, error) {
	observed, err := schema.ObservedTopic(current)
	if err != nil {
		return nil, err
	}
	intent, err := schema.DesiredTopic(desired)
	if err != nil {
		return nil, err
	}
	adopted, changed, err := topics.AdoptTransferConsumers(intent, observed)
	if err != nil || !changed {
		return desired, err
	}
	value, err := schema.CaptureTopicIntent(desired.GetOwner(), adopted)
	if err != nil {
		return nil, err
	}
	value.Context = proto.CloneOf(desired.GetContext())
	return value, nil
}

func equal(current, desired *protocol.Envelope) (bool, error) {
	before, after, err := schema.TopicPair(current, desired)
	if err != nil {
		return false, err
	}
	return topics.Equal(*after, *before), nil
}
