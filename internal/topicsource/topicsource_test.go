package topicsource_test

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
// overrides the entry's key. The model keeps the consumers in the order the
// document lists them, as the host's embedded engine wrote them.
func TestDecodeYAML_HappyPath(t *testing.T) {
	c := testcheck.New(t)
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
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true}, {Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}}})
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
			`topic "events", consumer "c": invalid availability_period "PT1H": .*`},
		{"two names for one topic", mapping(entry("a", mapping(entry("name", text("same")))), entry("b", mapping(entry("name", text("same"))))), `topic same is declared twice`},
		{"a slash in a name", mapping(entry("events", mapping(entry("name", text("a/b"))))), `topic "events": invalid name "a/b": holds a slash; name the directory with schema`},
		// Ported from the host's TestValidateIdentity_FailurePath and
		// TestParseConsumer_FailurePath.
		{"an empty name", mapping(entry("events", mapping(entry("name", text(" "))))), `.*invalid name: a topic needs a name`},
		{"a consumer with a slash in its name", mapping(entry("events", mapping(entry("consumers", mapping(entry("a/b", mapping())))))),
			`topic "events", consumer "a/b": invalid name "a/b": a consumer's name cannot hold a slash`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			values, err := topicsource.Decode(context.Background(), yamlDocument(test.value))
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}

// A setting a topic declaration refuses says what the setting takes: a count
// that is a whole number, a percentage from 1 to 100, a period of whole
// positive seconds, each codec once, and an auto-partitioning setting only
// beside a strategy that enables it. Ported from the host's ydbtopic
// TestParseTopic_FailurePath.
func TestDecodeYAMLRefusalSaysWhatTheSettingTakes_FailurePath(t *testing.T) {
	const shaping = "it shapes auto-partitioning, which this topic does not enable; declare " +
		"auto_partitioning_strategy as scale_up, scale_up_and_down or paused"
	tests := []struct {
		name     string
		settings []*protocol.SourceEntry
		want     string
	}{
		{name: "a count that is no number", settings: []*protocol.SourceEntry{entry("partition_write_burst_bytes", text("1MB"))},
			want: `invalid partition_write_burst_bytes "1MB": takes a whole number above zero`},
		{name: "a percentage over 100", settings: []*protocol.SourceEntry{entry("auto_partitioning_strategy", text("scale_up")),
			entry("auto_partitioning_up_utilization_percent", text("101"))},
			want: `invalid auto_partitioning_up_utilization_percent "101": takes a whole percentage from 1 to 100`},
		{name: "a fraction of a second", settings: []*protocol.SourceEntry{entry("retention_period", text("PT1.5S"))},
			want: `invalid retention_period "PT1.5S": YDB keeps whole seconds, and drops the fraction of this one`},
		{name: "no time at all", settings: []*protocol.SourceEntry{entry("retention_period", text("PT0S"))},
			want: `invalid retention_period "PT0S": is no positive time, and YDB takes only a positive interval`},
		{name: "a codec named twice", settings: []*protocol.SourceEntry{entry("supported_codecs", text("raw,RAW"))},
			want: `invalid supported_codecs "raw,RAW": names codec raw twice`},
		{name: "a threshold with the strategy disabled", settings: []*protocol.SourceEntry{entry("auto_partitioning_strategy", text("disabled")),
			entry("auto_partitioning_down_utilization_percent", text("10"))},
			want: "invalid auto_partitioning_down_utilization_percent: " + shaping},
		{name: "a window without a strategy", settings: []*protocol.SourceEntry{entry("auto_partitioning_stabilization_window", text("PT1M"))},
			want: "invalid auto_partitioning_stabilization_window: " + shaping},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			values, err := topicsource.Decode(context.Background(), yamlDocument(mapping(entry("events", mapping(test.settings...)))))

			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(`topic "events": `+test.want))
			c.Assert(values, qt.IsNil)
		})
	}
}

// A Go file declares a topic with ptah:schema:topic and each consumer with a
// ptah:schema:topic:consumer that names it, as the host's directives do; the
// consumer's directive itself contributes no value. The consumers keep the
// order of their directives.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
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
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true}, {Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}}})
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
		{"a consumer naming no topic", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c"}}}, `missing required annotation attribute "topic"`},
		{"a topic without a name", [][2]any{{topicsource.DirectiveTopic, map[string]string{"schema": "app"}}}, `missing required annotation attribute "name"`},
		{"a topic declared twice", [][2]any{topic, topic}, `topic events is declared twice`},
		{"an absolute directory", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "/local/app"}}}, `invalid schema "/local/app": starts with a slash; .*`},
		// Ported from the host's TestValidateIdentity_FailurePath,
		// TestDeclare_FailurePath and TestCheckDirectory_FailurePath: a topic's
		// name is one path segment, and its directory a clean path relative
		// to the database root.
		{"a parent segment as the name", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": ".."}}}, `invalid name "\.\.": is not a path segment`},
		{"an unclean directory", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "app//queues"}}},
			`invalid schema "app//queues": is not a directory path relative to the database root; write it without a trailing slash or an empty, \. or \.\. segment`},
		{"a trailing slash", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "app/"}}},
			`invalid schema "app/": is not a directory path relative to the database root; .*`},
		{"a parent directory", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "../app"}}},
			`invalid schema "\.\./app": is not a directory path relative to the database root; .*`},
		{"a consumer of a topic in an unclean directory", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events", "schema": "app//queues"}}},
			`invalid schema "app//queues": is not a directory path relative to the database root; .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			values, err := topicsource.Decode(context.Background(), goDocument(test.directives...))
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}
