package topicsource_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// A consumer declared above its topic, on another struct, is the topic's,
// in the order the file declares the consumers; a directory written with
// surrounding space names the directory; and a read_from written with an
// offset reads in UTC. Ported from the host's TestParseSource_Topic_HappyPath.
func TestDecodeGoConsumersWhereverTheyStand(t *testing.T) {
	c := testcheck.New(t)
	document := goDocument(
		[2]any{topicsource.DirectiveConsumer, map[string]string{"name": "audit", "topic": "events", "schema": "app", "read_from": "2026-01-01T03:00:00+03:00"}},
		[2]any{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": " app ", "retention_period": "PT36H", "supported_codecs": "raw,gzip"}},
		[2]any{topicsource.DirectiveConsumer, map[string]string{"name": "billing", "topic": "events", "schema": "app", "important": "true"}},
		[2]any{topicsource.DirectiveTopic, map[string]string{"name": "plain"}},
	)

	values, err := topicsource.Decode(context.Background(), document)

	c.Assert(err, qt.IsNil)
	c.Assert(values[1], qt.HasLen, 1)
	path, spec := declaredSpec(c, values[1][0])
	c.Assert(path, qt.Equals, "app/events")
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{RetentionPeriod: "P1DT12H", SupportedCodecs: []string{"raw", "gzip"},
		Consumers: []ydbdecl.Consumer{{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "billing", Important: true}}})
	c.Assert(values[3], qt.HasLen, 1)
	path, spec = declaredSpec(c, values[3][0])
	c.Assert(path, qt.Equals, "plain")
	c.Assert(spec, qt.DeepEquals, ydbdecl.Topic{})
}

// A topic or a consumer YDB would refuse or keep differently, and a consumer
// of a topic the file declares in another directory, are refused where they
// are written. Ported from the host's TestParseSource_Topic_FailurePath.
func TestDecodeGoTopicRows_FailurePath(t *testing.T) {
	topic := [2]any{topicsource.DirectiveTopic, map[string]string{"name": "events", "schema": "app"}}
	for _, test := range []struct {
		name       string
		directives [][2]any
		want       string
	}{
		{"a retention with a fraction of a second", [][2]any{{topicsource.DirectiveTopic, map[string]string{"name": "events", "retention_period": "PT1.5S"}}},
			`invalid retention_period "PT1.5S": YDB keeps whole seconds, and drops the fraction of this one`},
		{"a consumer of a topic in another directory", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events"}}},
			`the file declares no topic "events" for consumer "c"`},
		{"a consumer's directory with a trailing slash", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events", "schema": "app/"}}},
			`invalid schema "app/": is not a directory path relative to the database root; write it without a trailing slash or an empty, \. or \.\. segment`},
		{"a consumer's directory from the server root", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events", "schema": "/local/app"}}},
			`invalid schema "/local/app": starts with a slash; name the directory relative to the database root, without the database's own path`},
		{"a consumer important maybe", [][2]any{topic, {topicsource.DirectiveConsumer, map[string]string{"name": "c", "topic": "events", "schema": "app", "important": "maybe"}}},
			`invalid important "maybe": takes true or false`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			values, err := topicsource.Decode(context.Background(), goDocument(test.directives...))
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}
