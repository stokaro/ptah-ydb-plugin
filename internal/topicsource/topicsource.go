// Package topicsource decodes the standalone YDB topics a YAML document or a
// Go file declares, with the syntax and refusals of the host's bundled YDB
// source (internal/ydbsource): the YAML document key topics, and the Go
// directives ptah:schema:topic and ptah:schema:topic:consumer.
package topicsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// The syntax this package claims.
const (
	// KeyTopics is the YAML document key that declares topics.
	KeyTopics = "topics"
	// DirectiveTopic declares one topic in Go.
	DirectiveTopic = "ptah:schema:topic"
	// DirectiveConsumer declares one consumer of a topic the same Go file
	// declares.
	DirectiveConsumer = "ptah:schema:topic:consumer"
	attributeTopic    = "topic"
)

// topicSettings are the settings a topic declares, in the host's names.
func topicSettings() []string {
	return []string{ydbdecl.AttributeMinActivePartitions, ydbdecl.AttributeMaxActivePartitions, ydbdecl.AttributeStrategy,
		ydbdecl.AttributeUpUtilizationPercent, ydbdecl.AttributeDownUtilizationPercent, ydbdecl.AttributeStabilizationWindow,
		ydbdecl.AttributeRetentionPeriod, ydbdecl.AttributeWriteSpeed, ydbdecl.AttributeWriteBurst, ydbdecl.AttributeSupportedCodecs}
}

// consumerSettings are the settings a consumer declares.
func consumerSettings() []string {
	return []string{ydbdecl.AttributeImportant, ydbdecl.AttributeReadFrom, ydbdecl.AttributeSupportedCodecs, ydbdecl.AttributeAvailabilityPeriod}
}

// YAMLKey declares the YAML document key topics.
func YAMLKey() *protocol.YAMLSourceKey {
	return &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyTopics, Selector: KeyTopics, Models: []string{schema.TopicProperties}}
}

// GoDirectives declares the Go directives of a topic and of its consumers,
// with the attributes of the host's own directives of the same names.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveTopic, Selector: DirectiveTopic,
			Attributes: append([]string{ydbdecl.TopicName, ydbdecl.TopicSchema}, topicSettings()...), Models: []string{schema.TopicProperties}},
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveConsumer, Selector: DirectiveConsumer,
			Attributes: append([]string{ydbdecl.TopicName, attributeTopic, ydbdecl.TopicSchema}, consumerSettings()...), Models: []string{schema.TopicProperties}},
	}
}

// Owns reports whether a fragment carries one of this package's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_DOCUMENT && key.GetKey() == KeyTopics
	}
	directive := fragment.GetGoDirective().GetDirective()
	return directive == DirectiveTopic || directive == DirectiveConsumer
}

// declared is one topic a document declares, with the fragment and path its
// trace points at.
type declared struct {
	fragment int
	path     []*protocol.SourcePathStep
	schema   string
	name     string
	spec     ydbdecl.Topic
}

// Decode reads every topic a document declares and returns the values of
// each fragment, by index: a topic's value under the fragment that declares
// it, and none for a consumer's directive, whose consumer the topic carries.
// An invalid declaration refuses the whole document.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	var topicsDeclared []declared
	type pendingConsumer struct {
		schema, topic string
		consumer      ydbdecl.Consumer
	}
	var consumers []pendingConsumer
	owned := map[int]bool{}
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		owned[index] = true
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB topic source attachment")
		}
		mapping := fragment.GetValue().GetMapping()
		switch {
		case document.Format == protocol.SourceDocument_YAML && fragment.Selector == KeyTopics:
			if mapping == nil {
				return nil, fmt.Errorf("%s requires a mapping", KeyTopics)
			}
			read, err := yamlTopics(index, mapping)
			if err != nil {
				return nil, err
			}
			topicsDeclared = append(topicsDeclared, read...)
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == DirectiveTopic:
			values, err := attributes(mapping)
			if err != nil {
				return nil, err
			}
			spec, err := ydbdecl.ParseTopic(values)
			if err != nil {
				return nil, err
			}
			topicsDeclared = append(topicsDeclared, declared{fragment: index, schema: strings.TrimSpace(values[ydbdecl.TopicSchema]), name: strings.TrimSpace(values[ydbdecl.TopicName]), spec: spec})
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == DirectiveConsumer:
			values, err := attributes(mapping)
			if err != nil {
				return nil, err
			}
			consumer, err := ydbdecl.ParseConsumer(values)
			if err != nil {
				return nil, err
			}
			topic := strings.TrimSpace(values[attributeTopic])
			if topic == "" {
				return nil, fmt.Errorf("%s needs the topic it reads", DirectiveConsumer)
			}
			// The directory is held to the rules of a topic's, as the host
			// holds it.
			if err := ydbdecl.CheckTopic(values[ydbdecl.TopicSchema], topic, ydbdecl.Topic{}); err != nil {
				return nil, err
			}
			consumers = append(consumers, pendingConsumer{schema: strings.TrimSpace(values[ydbdecl.TopicSchema]), topic: topic, consumer: consumer})
		default:
			return nil, fmt.Errorf("unsupported YDB topic source format")
		}
	}
	// A consumer of a topic the file does not declare is refused rather than
	// dropped, because a reader nobody creates is a declaration with no
	// effect, and so is a second consumer of one name, which YDB refuses.
	for _, pending := range consumers {
		index := slices.IndexFunc(topicsDeclared, func(topic declared) bool { return topic.name == pending.topic && topic.schema == pending.schema })
		if index < 0 {
			return nil, fmt.Errorf("the file declares no topic %q for consumer %q", topics.Display(pending.schema, pending.topic), pending.consumer.Name)
		}
		topic := &topicsDeclared[index]
		if slices.ContainsFunc(topic.spec.Consumers, func(have ydbdecl.Consumer) bool { return have.Name == pending.consumer.Name }) {
			return nil, fmt.Errorf("topic %q declares consumer %q twice", topics.Display(topic.schema, topic.name), pending.consumer.Name)
		}
		topic.spec.Consumers = append(topic.spec.Consumers, pending.consumer)
	}
	result := make(map[int][]*protocol.SourceModelValue, len(owned))
	for index := range owned {
		result[index] = nil
	}
	seen := map[string]bool{}
	for position, topic := range topicsDeclared {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := ydbdecl.CheckTopic(topic.schema, topic.name, topic.spec); err != nil {
			return nil, err
		}
		path := topics.Display(topic.schema, topic.name)
		if seen[path] {
			return nil, fmt.Errorf("topic %s is declared twice", path)
		}
		seen[path] = true
		intent, err := topics.Intent(topic.spec)
		if err != nil {
			return nil, err
		}
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: topic.schema, Name: topic.name}
		value, err := schema.CaptureTopicIntent(owner, intent)
		if err != nil {
			return nil, fmt.Errorf("topic %s: %w", path, err)
		}
		fragment := document.Fragments[topic.fragment]
		result[topic.fragment] = append(result[topic.fragment], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: "topic." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(owner),
				CanonicalFormat: schema.NativeDesiredModel(schema.TopicProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: topic.path}})
	}
	return result, nil
}

// yamlTopics reads the topics mapping of a YAML document: each entry a topic
// keyed by its name, which a name key overrides, in key order.
func yamlTopics(fragment int, mapping *protocol.SourceMapping) ([]declared, error) {
	entries := slices.Clone(mapping.GetEntries())
	slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
	var result []declared
	for _, entry := range entries {
		topic, err := yamlTopic(entry)
		if err != nil {
			return nil, fmt.Errorf("topic %q: %w", entry.Key, err)
		}
		topic.fragment = fragment
		topic.path = []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}
		result = append(result, topic)
	}
	return result, nil
}

func yamlTopic(entry *protocol.SourceEntry) (declared, error) {
	fields := entry.GetValue().GetMapping()
	if fields == nil {
		return declared{}, fmt.Errorf("a topic requires a mapping")
	}
	values := map[string]string{}
	var consumerEntries []*protocol.SourceEntry
	topic := declared{name: entry.Key}
	for _, field := range fields.GetEntries() {
		switch {
		case field.Key == "consumers":
			consumerMapping := field.GetValue().GetMapping()
			if consumerMapping == nil && field.GetValue().GetNull() == nil {
				return declared{}, fmt.Errorf("consumers requires a mapping")
			}
			consumerEntries = consumerMapping.GetEntries()
		case field.Key == ydbdecl.AttributeSupportedCodecs:
			list, err := codecList(field.GetValue())
			if err != nil {
				return declared{}, fmt.Errorf("%s: %w", field.Key, err)
			}
			values[field.Key] = list
		case field.Key == ydbdecl.TopicName || field.Key == ydbdecl.TopicSchema || slices.Contains(topicSettings(), field.Key):
			text, err := scalar(field.GetValue())
			if err != nil {
				return declared{}, fmt.Errorf("%s: %w", field.Key, err)
			}
			values[field.Key] = text
		default:
			return declared{}, fmt.Errorf("unknown key %q", field.Key)
		}
	}
	if name := values[ydbdecl.TopicName]; name != "" {
		topic.name = name
	}
	topic.schema = values[ydbdecl.TopicSchema]
	delete(values, ydbdecl.TopicName)
	delete(values, ydbdecl.TopicSchema)
	spec, err := ydbdecl.ParseTopic(values)
	if err != nil {
		return declared{}, err
	}
	for _, consumerEntry := range consumerEntries {
		consumer, err := yamlConsumer(consumerEntry)
		if err != nil {
			return declared{}, fmt.Errorf("consumer %q: %w", consumerEntry.Key, err)
		}
		spec.Consumers = append(spec.Consumers, consumer)
	}
	topic.spec = spec
	return topic, nil
}

func yamlConsumer(entry *protocol.SourceEntry) (ydbdecl.Consumer, error) {
	values := map[string]string{}
	if entry.GetValue().GetNull() == nil {
		fields := entry.GetValue().GetMapping()
		if fields == nil {
			return ydbdecl.Consumer{}, fmt.Errorf("a consumer requires a mapping")
		}
		for _, field := range fields.GetEntries() {
			switch {
			case field.Key == ydbdecl.AttributeSupportedCodecs:
				list, err := codecList(field.GetValue())
				if err != nil {
					return ydbdecl.Consumer{}, fmt.Errorf("%s: %w", field.Key, err)
				}
				values[field.Key] = list
			case slices.Contains(consumerSettings(), field.Key):
				text, err := scalar(field.GetValue())
				if err != nil {
					return ydbdecl.Consumer{}, fmt.Errorf("%s: %w", field.Key, err)
				}
				values[field.Key] = text
			default:
				return ydbdecl.Consumer{}, fmt.Errorf("unknown key %q", field.Key)
			}
		}
	}
	values[ydbdecl.TopicName] = entry.Key
	return ydbdecl.ParseConsumer(values)
}

// attributes reads a Go directive's attributes, which the host lexed as text.
func attributes(mapping *protocol.SourceMapping) (map[string]string, error) {
	if mapping == nil {
		return nil, fmt.Errorf("a topic directive requires attributes")
	}
	values := map[string]string{}
	for _, entry := range mapping.GetEntries() {
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, fmt.Errorf("attribute %s requires text", entry.Key)
		}
		values[entry.Key] = text.Text
	}
	return values, nil
}

// scalar reads a YAML scalar as the text the host's bundled source reads it
// as: a number keeps its digits, a boolean reads true or false, and a null
// is a value written empty, which the setting's parse refuses.
func scalar(value *protocol.SourceValue) (string, error) {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return content.Text, nil
	case *protocol.SourceValue_Number:
		return content.Number, nil
	case *protocol.SourceValue_Boolean:
		return strconv.FormatBool(content.Boolean), nil
	case *protocol.SourceValue_Null:
		return "", nil
	default:
		return "", fmt.Errorf("expected a scalar")
	}
}

// codecList reads a codec list written as a sequence or as one
// comma-separated scalar, as the host reads one.
func codecList(value *protocol.SourceValue) (string, error) {
	sequence := value.GetSequence()
	if sequence == nil {
		return scalar(value)
	}
	var names []string
	for _, item := range sequence.GetValues() {
		text, err := scalar(item)
		if err != nil {
			return "", err
		}
		names = append(names, text)
	}
	return strings.Join(names, ","), nil
}
