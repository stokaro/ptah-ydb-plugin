package sourceencode

import (
	"fmt"
	"path"
	"strconv"

	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Topic writes a topic's settings and every consumer: in YAML one entry of the
// topics key with its consumers inside it, in Go one topic directive and one
// consumer directive for each consumer. A topic is written as the declaration
// the topic comparison reads it as (topics.FromIntent): the attributes YDB
// keeps on a topic and its consumers are not declared, and a setting a read
// reports at YDB's value is written as that value. A setting no YQL statement
// sets, and an access requirement, are losses.
func Topic(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredTopic(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	owner := value.GetValue().GetOwner()
	if intent.GetAccess() != nil {
		return AccessLoss("topic", owner), nil
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	spec, err := topics.FromIntent(intent)
	if err != nil {
		return loss("topic %s: %v", Subject(owner), err), nil
	}
	fields = append(fields, sortedFields(ydbdecl.TopicValues(spec))...)
	first := &protocol.SourceFragment{Id: "native." + value.GetId()}
	fragments := []*protocol.SourceFragment{first}
	switch format {
	case protocol.SourceDocument_YAML:
		var consumers []*protocol.SourceEntry
		for _, consumer := range spec.Consumers {
			attributes := ydbdecl.ConsumerValues(consumer)
			delete(attributes, ydbdecl.TopicName)
			consumers = append(consumers, &protocol.SourceEntry{Key: consumer.Name, Value: mapping(sortedFields(attributes)...)})
		}
		if len(consumers) > 0 {
			fields = append(fields, &protocol.SourceEntry{Key: "consumers", Value: mapping(consumers...)})
		}
		first.YamlKey = topicsource.YAMLKey()
		first.Selector = first.YamlKey.Selector
		first.Content = &protocol.SourceFragment_Value{Value: mapping(&protocol.SourceEntry{Key: path.Join(owner.Schema, owner.Name), Value: mapping(fields...)})}
	case protocol.SourceDocument_GO_ANNOTATIONS:
		directives := topicsource.GoDirectives()
		first.GoDirective, first.Selector = directives[0], directives[0].Selector
		first.Content = &protocol.SourceFragment_Value{Value: mapping(fields...)}
		for i, consumer := range spec.Consumers {
			attributes := ydbdecl.ConsumerValues(consumer)
			attributes["topic"] = owner.Name
			if owner.Schema != "" {
				attributes[ydbdecl.TopicSchema] = owner.Schema
			}
			fragments = append(fragments, &protocol.SourceFragment{Id: first.Id + ".consumer." + strconv.Itoa(i),
				GoDirective: directives[1], Selector: directives[1].Selector, Content: &protocol.SourceFragment_Value{Value: mapping(sortedFields(attributes)...)}})
		}
	case protocol.SourceDocument_HCL:
		return HCLLoss(value.GetValue()), nil
	default:
		return Result{}, fmt.Errorf("unsupported topic source format")
	}
	return nativeParts(value, format, fragments, topicsource.Decode, sameTopic)
}

// sameTopic reports two topics the topic comparison finds equal.
func sameTopic(before, after *protocol.Envelope) (bool, error) {
	want, err := schema.DesiredTopic(before)
	if err != nil {
		return false, err
	}
	got, err := schema.DesiredTopic(after)
	if err != nil {
		return false, err
	}
	wanted, err := topics.FromIntent(want)
	if err != nil {
		return false, err
	}
	read, err := topics.FromIntent(got)
	if err != nil {
		return false, nil
	}
	return topics.Equal(read, wanted), nil
}
