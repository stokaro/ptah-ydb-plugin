package schema

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// TopicChange identifies a complete before/after change of one topic.
	TopicChange = modelNamespace + "topic-change"
	// TopicOperation identifies the executable change of one topic.
	TopicOperation = modelNamespace + "topic-operation"
)

// The reasons a topic change gives for its impact, the host's words.
const (
	dropTopicReason    = "DROP TOPIC removes the topic, every message it holds and every consumer's position in it"
	dropConsumerReason = "DROP CONSUMER removes a topic consumer and its position in the topic"
	changeTopicReason  = "ALTER TOPIC can shorten how long the topic keeps a message, or move where a consumer reads from"
)

var topicChanges = newObjectChanges(objectChanges{kind: TopicProperties, change: TopicChange, operation: TopicOperation, noun: "topic",
	semantics: topicSemantics,
	definition: map[string]any{
		"semantics":  "create: additive, access unknown; drop: destructive, removes the topic's messages and consumer positions, narrows access; alter dropping or restarting a consumer: destructive, removes its position; alter adding consumers only: additive; other alter: behavioral; alter keeps access; never replaces owner",
		"comparison": "both operands read as the host's topic specification: settings resolved to the values YDB gives a new topic, consumers by name; attributes and access metadata are not compared",
		"validation": "a pair that holds no change is refused",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE TOPIC, ALTER TOPIC or DROP TOPIC statements, one query each, outside SQL transactions; no automatic retry",
		"precondition": "before is the complete normalized observation or established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the settings and consumers the desired side resolves to, unchanged access metadata after an alter, or absence after a drop",
		"database":     "a nonempty owner database must equal the selected session database; empty selects that session database",
	}})

// TopicChangeModel defines a topic change and its safety semantics.
func TopicChangeModel() *protocol.ValueModel { return proto.CloneOf(topicChanges.changeModel) }

// TopicOperationModel defines a topic operation and its preconditions.
func TopicOperationModel() *protocol.ValueModel { return proto.CloneOf(topicChanges.operationModel) }

// CaptureTopicChange retains complete operands under one owner; either may be
// nil for an established absence.
func CaptureTopicChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return topicChanges.capture(id, owner, before, after)
}

// NormalizeTopicChange reconstructs a change's safety facts from its operands.
func NormalizeTopicChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return topicChanges.normalizeChange(input)
}

// TopicOperands returns a topic change's normalized operands, nil for an
// absent one.
func TopicOperands(change *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	return topicChanges.operands(change)
}

// TopicOperationFromChange lowers a complete change to its operation.
func TopicOperationFromChange(id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return topicChanges.operationFromChange(id, change)
}

// NormalizeTopicOperation reconstructs an operation's facts and refuses a
// no-op.
func NormalizeTopicOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return topicChanges.normalizeOperation(input)
}

// TopicOperationChange restores the change an operation carries.
func TopicOperationChange(operation *protocol.Envelope) *protocol.Envelope {
	return topicChanges.operationChange(operation)
}

// TopicSpecs reads a topic change's operands as the host's topic
// specifications, nil for an absent one.
func TopicSpecs(change *protocol.Envelope) (*ydbdecl.Topic, *ydbdecl.Topic, error) {
	before, after, err := TopicOperands(change)
	if err != nil {
		return nil, nil, err
	}
	return topicOperandSpecs(before, after)
}

func topicOperandSpecs(before, after *protocol.Envelope) (*ydbdecl.Topic, *ydbdecl.Topic, error) {
	var current, desired *ydbdecl.Topic
	if before != nil {
		observed, err := ObservedTopic(before)
		if err != nil {
			return nil, nil, err
		}
		spec, err := topics.FromObserved(observed)
		if err != nil {
			return nil, nil, fmt.Errorf("topic %s: %w", topics.Display(before.Owner.GetSchema(), before.Owner.GetName()), err)
		}
		current = &spec
	}
	if after != nil {
		intent, err := DesiredTopic(after)
		if err != nil {
			return nil, nil, err
		}
		spec, err := topics.FromIntent(intent)
		if err != nil {
			return nil, nil, fmt.Errorf("topic %s: %w", topics.Display(after.Owner.GetSchema(), after.Owner.GetName()), err)
		}
		desired = &spec
	}
	return current, desired, nil
}

// ObservedTopic decodes a normalized topic observation.
func ObservedTopic(value *protocol.Envelope) (*native.DescribeTopicResult, error) {
	normalized, err := Normalize(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != TopicProperties {
		return nil, fmt.Errorf("expected a YDB topic observation")
	}
	var observed native.DescribeTopicResult
	if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
		return nil, err
	}
	return &observed, nil
}

// DesiredTopic decodes a normalized topic declaration.
func DesiredTopic(value *protocol.Envelope) (*native.TopicIntent, error) {
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != TopicProperties {
		return nil, fmt.Errorf("expected a YDB topic declaration")
	}
	var intent native.TopicIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return nil, err
	}
	return &intent, nil
}

// CaptureTopicIntent writes a declared topic as the provider's desired model
// under owner.
func CaptureTopicIntent(owner *protocol.Reference, intent *native.TopicIntent) (*protocol.Envelope, error) {
	definition := NativeDesiredModel(TopicProperties)
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: TopicProperties, Owner: proto.CloneOf(owner)}, definition, intent, NormalizeDesired)
}

func topicSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	current, desired, err := topicOperandSpecs(before, after)
	if err != nil {
		return nil, err
	}
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Altering a topic preserves its access metadata"}
	switch {
	case current == nil:
		semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_ADDITIVE, "CREATE TOPIC adds a topic", protocol.ChangeSemantics_ACCESS_UNKNOWN, "Creation inherits server access policy"
	case desired == nil:
		semantics.RemovesState, semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, dropTopicReason, protocol.ChangeSemantics_NARROWS, "Dropping the topic removes access to it"
	case topics.Equal(*desired, *current):
		return nil, fmt.Errorf("topic operands contain no change")
	default:
		consumers := topics.Compare(*desired, *current)
		switch {
		case len(consumers.Removed)+len(consumers.Restarted) > 0:
			semantics.RemovesState, semantics.Impact, semantics.Reason = new(true), protocol.ChangeSemantics_DESTRUCTIVE, dropConsumerReason
		case topics.SettingsEqual(*desired, *current) && len(consumers.Changed) == 0:
			semantics.Impact, semantics.Reason = protocol.ChangeSemantics_ADDITIVE, "ALTER TOPIC adds consumers"
		default:
			semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, changeTopicReason
		}
	}
	return semantics, nil
}

// ValidateTopicAccess checks a declaration's access requirements against the
// access the topic holds. ALTER TOPIC keeps the access entries, and CREATE
// TOPIC cannot promise any.
func ValidateTopicAccess(before, after *protocol.Envelope) error {
	if after == nil {
		return nil
	}
	intent, err := DesiredTopic(after)
	if err != nil {
		return err
	}
	required := intent.GetAccess()
	if required == nil {
		return nil
	}
	if before == nil {
		return fmt.Errorf("topic creation cannot establish requested access requirements")
	}
	observed, err := ObservedTopic(before)
	if err != nil {
		return err
	}
	actual := accessIntent(observed.GetSelf())
	if actual == nil || (required.Owner != nil && required.GetOwner() != actual.GetOwner()) ||
		!permissionSetsEqual(required.DirectGrants, actual.DirectGrants) || !permissionSetsEqual(required.RequiredEffectivePermissions, actual.RequiredEffectivePermissions) {
		return fmt.Errorf("topic access requirements differ from established access")
	}
	return nil
}

// TopicPair reads a read topic and its declaration as the host's topic
// specifications.
func TopicPair(current, desired *protocol.Envelope) (*ydbdecl.Topic, *ydbdecl.Topic, error) {
	return topicOperandSpecs(current, desired)
}
