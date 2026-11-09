// Package topiccompare compares the declared and the read standalone YDB
// topics offline, as Ptah's embedded YDB engine compares them: each setting
// resolved to the value YDB gives a new topic, and the consumers by name.
package topiccompare

import (
	"context"
	"fmt"
	"path"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the owner of topic comparisons.
const ServiceID = "ydb-topics"

// Compare evaluates complete operands and coverage without database access.
// A topic holding a setting no declaration states refuses the comparison, as
// the host refuses to read one, with its words (see [readRefusal]), whether
// or not the source describes the topic.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, Family())
}

// Family is the topic's part of a comparison.
func Family() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.TopicProperties, ChangeKind: schema.TopicChange, Capability: topics.CapabilityTopics, Noun: "topic", DottedLimits: true,
		// YDB drops a topic a transfer reads and keeps the transfer, which
		// then fails (`Discovery for all topics failed`), measured on
		// 26.2.1.14. The host's embedded engine refused such a plan.
		RequiredRemoval: "is dropped while a declared transfer reads it, and YDB keeps the transfer, which then stops " +
			"(`Discovery for all topics failed`); declare the topic, or remove the transfer",
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.TopicProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.TopicProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: schema.ValidateTopicAccess,
		Adopt:     adopt,
		Equal:     equal,
		Change:    change,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			observed, err := schema.ObservedTopic(current)
			if err != nil {
				return nil, readRefusal(current, err)
			}
			intent, err := topics.Keep(observed)
			if err != nil {
				return nil, readRefusal(current, err)
			}
			return schema.CaptureTopicIntent(owner, intent)
		},
	}
}

// readRefusal refuses a comparison over the read topic current, which the
// provider cannot describe for why, in the words the host's embedded engine
// refused to read such a topic with, `YDB topic <path>: <why>`, the path from
// the database root, as the provider names a table it cannot drop. The reason
// is authored, so the host prints it; without that it prints `provider
// compare models failed (InvalidArgument)` alone, and the author cannot tell
// which topic stops the run.
func readRefusal(current *protocol.Envelope, why error) error {
	owner := current.GetOwner()
	return authored.Wrap(why, fmt.Sprintf("YDB topic %s: %v", path.Join(owner.GetSchema(), owner.GetName()), why))
}

// describable refuses the read topic current where no declaration can
// describe it (see [readRefusal]).
func describable(current *protocol.Envelope) error {
	observed, err := schema.ObservedTopic(current)
	if err != nil {
		return readRefusal(current, err)
	}
	if _, err := topics.FromObserved(observed); err != nil {
		return readRefusal(current, err)
	}
	return nil
}

// adopt gives the declared topic the consumers a transfer of the read reads
// it through, which the declaration cannot name; see
// [topics.AdoptTransferConsumers].
func adopt(current, desired *protocol.Envelope) (*protocol.Envelope, error) {
	observed, err := schema.ObservedTopic(current)
	if err != nil {
		return nil, readRefusal(current, err)
	}
	intent, err := schema.DesiredTopic(desired)
	if err != nil {
		return nil, err
	}
	adopted, changed, err := topics.AdoptTransferConsumers(intent, observed)
	if err != nil {
		return nil, readRefusal(current, err)
	}
	if !changed {
		return desired, nil
	}
	value, err := schema.CaptureTopicIntent(desired.GetOwner(), adopted)
	if err != nil {
		return nil, err
	}
	value.Context = proto.CloneOf(desired.GetContext())
	return value, nil
}

// change is the change from the read topic current to the declared one,
// either of them absent for a creation or a removal.
func change(id string, owner *protocol.Reference, current, desired *protocol.Envelope) (*protocol.NormalizedValue, error) {
	if current != nil {
		if err := describable(current); err != nil {
			return nil, err
		}
	}
	return schema.CaptureTopicChange(id, owner, current, desired)
}

func equal(current, desired *protocol.Envelope) (bool, error) {
	if err := describable(current); err != nil {
		return false, err
	}
	before, after, err := schema.TopicPair(current, desired)
	if err != nil {
		return false, err
	}
	return topics.Equal(*after, *before), nil
}
