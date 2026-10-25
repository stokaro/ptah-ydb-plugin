package topicsource_test

import (
	"context"
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

// mapping builds a captured mapping from entries in written order.
func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func entry(key string, value *protocol.SourceValue) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: value}
}

func yamlDocument(topicsValue *protocol.SourceValue) *protocol.SourceDocument {
	return &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_YAML, Models: []*protocol.ValueModel{schema.NativeDesiredModel(schema.TopicProperties)},
		Fragments: []*protocol.SourceFragment{{Id: "topics", Selector: topicsource.KeyTopics, YamlKey: topicsource.YAMLKey(), Content: &protocol.SourceFragment_Value{Value: topicsValue}}}}
}

// goDocument captures one Go file's topic and consumer directives, each a
// mapping of text attributes.
func goDocument(directives ...[2]any) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_GO_ANNOTATIONS, Models: []*protocol.ValueModel{schema.NativeDesiredModel(schema.TopicProperties)}}
	for i, directive := range directives {
		name := directive[0].(string)
		var entries []*protocol.SourceEntry
		for key, value := range directive[1].(map[string]string) {
			entries = append(entries, entry(key, text(value)))
		}
		claim := topicsource.GoDirectives()[0]
		if name == topicsource.DirectiveConsumer {
			claim = topicsource.GoDirectives()[1]
		}
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + i)), Selector: name, GoDirective: claim,
			Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}})
	}
	return document
}

// declaredSpec reads the topic one value declares as the host's spec.
func declaredSpec(c *qt.C, value *protocol.SourceModelValue) (string, ydbdecl.Topic) {
	c.Helper()
	intent, err := schema.DesiredTopic(value.Value.Value)
	c.Assert(err, qt.IsNil)
	spec, err := topics.FromIntent(intent)
	c.Assert(err, qt.IsNil)
	return topics.Display(value.Value.Value.Owner.Schema, value.Value.Value.Owner.Name), spec
}

// A YAML document declares topics with the host's keys: settings as text,
// numbers or booleans, a codec list as a sequence, and consumers. A name key
// overrides the entry's key. The model keeps consumers sorted by name.
func TestDecodeYAML_HappyPath(t *testing.T) {
	c := qt.New(t)
	number := func(value string) *protocol.SourceValue {
		return &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: value}}
	}
	boolean := &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: true}}
	codecs := &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: []*protocol.SourceValue{text("raw"), text("gzip")}}}}
	document := yamlDocument(mapping(
		entry("order_events", mapping(entry("schema", text("app")), entry("min_active_partitions", number("2")), entry("retention_period", text("PT36H")),
			entry("supported_codecs", codecs),
			entry("consumers", mapping(entry("billing", mapping(entry("important", boolean))), entry("audit", mapping(entry("read_from", text("2026-01-01T00:00:00Z")))))))),
		entry("plain", mapping(entry("name", text("renamed")))),
	))
	values, err := topicsource.Decode(context.Background(), document)
	c.Assert(err, qt.IsNil)
	c.Assert(values[0], qt.HasLen, 2)
	path, spec := declaredSpec(c, values[0][0])
	c.Assert(path, qt.Equals, "app/order_events")
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 2, RetentionPeriod: "P1DT12H", SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "billing", Important: true}}})
	c.Assert(values[0][0].Source.Path[0].GetKey(), qt.Equals, "order_events")
	path, spec = declaredSpec(c, values[0][1])
	c.Assert(path, qt.Equals, "renamed")
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{})
}

func TestDecodeYAML_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		value *protocol.SourceValue
		want  string
	}{
		{"an unknown key", mapping(entry("events", mapping(entry("retention_storage_mb", text("5"))))), `topic "events": unknown key "retention_storage_mb"`},
		{"a shaping setting without a strategy", mapping(entry("events", mapping(entry("max_active_partitions", text("4"))))),
			`topic "events": invalid max_active_partitions: it shapes auto-partitioning, which this topic does not enable; .*`},
		{"an empty count", mapping(entry("events", mapping(entry("min_active_partitions", &protocol.SourceValue{Value: &protocol.SourceValue_Null{Null: &protocol.Empty{}}})))),
			`topic "events": invalid min_active_partitions: takes a whole number above zero`},
		{"an important consumer with an availability period", mapping(entry("events", mapping(entry("consumers", mapping(entry("c", mapping(entry("important", text("true")), entry("availability_period", text("PT1H"))))))))),
			`topic "events": consumer "c": invalid availability_period "PT1H": .*`},
		{"two names for one topic", mapping(entry("a", mapping(entry("name", text("same")))), entry("b", mapping(entry("name", text("same"))))), `topic same is declared twice`},
		{"a slash in a name", mapping(entry("events", mapping(entry("name", text("a/b"))))), `invalid name "a/b": holds a slash; name the directory with schema`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := topicsource.Decode(context.Background(), yamlDocument(test.value))
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}

// A Go file declares a topic with ptah:schema:topic and each consumer with a
// ptah:schema:topic:consumer that names it, as the host's directives do; the
// consumer's directive itself contributes no value.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := qt.New(t)
	document := goDocument(
		[2]any{topicsource.DirectiveTopic, map[string]string{"name": "order_events", "schema": "app", "min_active_partitions": "2", "retention_period": "PT36H", "supported_codecs": "raw,gzip"}},
		[2]any{topicsource.DirectiveConsumer, map[string]string{"name": "billing", "topic": "order_events", "schema": "app", "important": "true"}},
		[2]any{topicsource.DirectiveConsumer, map[string]string{"name": "audit", "topic": "order_events", "schema": "app", "read_from": "2026-01-01T00:00:00Z"}},
	)
	values, err := topicsource.Decode(context.Background(), document)
	c.Assert(err, qt.IsNil)
	c.Assert(values[0], qt.HasLen, 1)
	c.Assert(values[1], qt.HasLen, 0)
	c.Assert(values[2], qt.HasLen, 0)
	path, spec := declaredSpec(c, values[0][0])
	c.Assert(path, qt.Equals, "app/order_events")
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{MinActivePartitions: 2, RetentionPeriod: "P1DT12H", SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "billing", Important: true}}})
	payload, err := json.Marshal(spec)
	c.Assert(err, qt.IsNil)
	c.Assert(string(payload), qt.Contains, `"consumers"`)
}

func TestDecodeGo_FailurePath(t *testing.T) {
	topic := [2]any{topicsource.DirectiveTopic, map[string]string{"name": "events"}}
	for _, test := range []struct {
		name       string
		directives [][2]any
		want       string
	}{
		{"a consumer of a topic the file does not declare", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "other"}}},
			`the file declares no topic "other" for consumer "c"`},
		{"a consumer declared twice", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events"}}, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events"}}},
			`topic "events" declares consumer "c" twice`},
		{"a consumer naming no topic", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c"}}}, `ptah:schema:topic:consumer needs the topic it reads`},
		{"a topic without a name", [][2]any{{topicsource.DirectiveTopic, map[string]string{"schema": "app"}}}, `invalid name: a topic needs a name`},
		{"a topic declared twice", [][2]any{topic, topic}, `topic events is declared twice`},
		{"an absolute directory", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "/local/app"}}}, `invalid schema "/local/app": starts with a slash; .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := topicsource.Decode(context.Background(), goDocument(test.directives...))
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}
