package yqlschema

import (
	"maps"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// topic reads `CREATE TOPIC <path> (CONSUMER ...) WITH (...)`. A dot is part
// of a name and a path written absolute is refused.
func (p *parser) topic() *createTopic {
	ref, err := ydbdecl.ParseTopicPath(decodedName(p.path()))
	if err != nil {
		p.failf("%v", err)
	}
	var consumers []ydbdecl.Consumer
	seen := make(map[string]bool)
	if p.accept("(") {
		for !p.done() {
			p.wantWord("CONSUMER")
			consumer := p.consumer()
			if seen[consumer.Name] {
				p.failf("consumer %q is declared twice", consumer.Name)
			}
			seen[consumer.Name] = true
			consumers = append(consumers, consumer)
			if !p.accept(",") {
				break
			}
		}
		p.want(")")
	}
	spec, err := ydbdecl.ParseTopic(p.declarationSettings(topicSetting))
	if err != nil {
		p.failf("%v", err)
	}
	spec.Consumers = consumers
	return &createTopic{schema: ref.Schema, name: ref.Name, spec: spec}
}

func (p *parser) consumer() ydbdecl.Consumer {
	name := decodedName(p.identifier())
	values := p.declarationSettings(consumerSetting)
	values[ydbdecl.TopicName] = name
	spec, err := ydbdecl.ParseConsumer(values)
	if err != nil {
		p.failf("%v", err)
	}
	if spec.Name != name {
		p.failf("consumer name %q cannot be represented exactly", name)
	}
	return spec
}

func (p *parser) declarationSettings(setting func(string) string) map[string]string {
	values := make(map[string]string)
	if !p.word("WITH") {
		return values
	}
	p.pos++
	raw := p.options()
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		value := raw[key]
		constructor := setting(key)
		switch constructor {
		case "unsupported":
			p.failf("unsupported declaration setting %q", key)
		case "":
			values[key] = scalar(value)
		default:
			literal := newParser(value)
			literal.wantWord(constructor)
			literal.want("(")
			decoded, ok := stringLiteral(literal.expression(), "String")
			literal.want(")")
			if literal.err != nil || !literal.done() || !ok {
				p.failf("%s requires %s with a string literal", key, constructor)
			}
			values[key] = decoded
		}
	}
	return values
}

// topicSetting is the syntax boundary; the declaration codecs validate
// values.
func topicSetting(key string) string {
	switch key {
	case ydbdecl.AttributeMinActivePartitions, ydbdecl.AttributeMaxActivePartitions,
		ydbdecl.AttributeStrategy, ydbdecl.AttributeUpUtilizationPercent,
		ydbdecl.AttributeDownUtilizationPercent, ydbdecl.AttributeWriteSpeed, ydbdecl.AttributeWriteBurst,
		ydbdecl.AttributeSupportedCodecs:
		return ""
	case ydbdecl.AttributeStabilizationWindow, ydbdecl.AttributeRetentionPeriod:
		return "Interval"
	default:
		return "unsupported"
	}
}

func consumerSetting(key string) string {
	switch key {
	case ydbdecl.AttributeImportant, ydbdecl.AttributeSupportedCodecs:
		return ""
	case ydbdecl.AttributeReadFrom:
		return "Timestamp"
	case ydbdecl.AttributeAvailabilityPeriod:
		return "Interval"
	default:
		return "unsupported"
	}
}
