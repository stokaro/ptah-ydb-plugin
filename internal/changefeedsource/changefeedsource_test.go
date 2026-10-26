package changefeedsource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
)

// declared decodes a request through the provider's source service and reads
// back the changefeeds each table's facet declares, by table.
func declared(c *qt.C, request *protocol.DecodeSourceRequest) map[string][]changefeeds.Feed {
	c.Helper()
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	result := map[string][]changefeeds.Feed{}
	for _, fragment := range decoded.Documents[0].Fragments {
		for _, value := range fragment.Values {
			envelope := value.Value.Value
			if envelope.Kind != schema.ChangefeedProperties {
				continue
			}
			c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_FACET)
			var set native.ChangefeedSetIntent
			c.Assert(protojson.Unmarshal(envelope.Payload, &set), qt.IsNil)
			feeds, err := changefeeds.Declared(&set)
			c.Assert(err, qt.IsNil)
			result[envelope.Owner.Name] = feeds
		}
	}
	return result
}

// feed is a declared changefeed as the decoders read it.
func feed(spec ydbdecl.Changefeed) changefeeds.Feed { return changefeeds.Feed{Changefeed: spec} }

func TestClaims_HappyPath(t *testing.T) {
	c := qt.New(t)
	key := changefeedsource.YAMLKey()
	c.Assert(key.Scope, qt.Equals, protocol.YAMLSourceKey_TABLE)
	c.Assert(key.Key, qt.Equals, "changefeeds")
	c.Assert(key.Models, qt.DeepEquals, []string{schema.ChangefeedProperties})
	directives := changefeedsource.GoDirectives()
	c.Assert(directives, qt.HasLen, 2)
	c.Assert(directives[0].Directive, qt.Equals, "ptah:schema:changefeed")
	c.Assert(directives[0].Attributes, qt.DeepEquals, []string{"name", "table", "mode", "format", "virtual_timestamps", "resolved_timestamps",
		"initial_scan", "user_sids", "schema_changes", "topic_min_active_partitions", "topic_auto_partitioning", "retention_period"})
	c.Assert(directives[1].Directive, qt.Equals, "ptah:schema:changefeed:consumer")
	c.Assert(directives[1].Attributes, qt.DeepEquals, []string{"name", "changefeed", "table", "important", "read_from", "supported_codecs", "availability_period"})
	for _, directive := range directives {
		c.Assert(directive.Scope, qt.Equals, protocol.GoSourceDirective_TABLE)
		c.Assert(directive.Models, qt.DeepEquals, []string{schema.ChangefeedProperties})
	}
	var claimed []string
	for _, candidate := range backend.Description().Engines[0].YamlSourceKeys {
		claimed = append(claimed, candidate.Key)
	}
	c.Assert(claimed, qt.Contains, "changefeeds")
}

// A YAML table declares its changefeeds keyed by name, with each option keyed
// as the annotation keys it and the consumers of its topic keyed by name. The
// facet keeps the changefeeds and each topic's consumers by name.
func TestDecodeYAML_HappyPath(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.YAMLTables(`{"tables": {"items": {"columns": {"id": {"type": "bigint", "primary": true}}, "changefeeds": {
		"updates": {"mode": "updates", "format": "JSON", "retention_period": "PT12H", "initial_scan": true, "topic_min_active_partitions": 2,
			"consumers": {"billing": {"important": true}, "audit": {"supported_codecs": ["raw", "gzip"], "read_from": "2026-01-01T00:00:00Z"}}},
		"keys": {"mode": "KEYS_ONLY", "format": "json", "virtual_timestamps": "true", "resolved_timestamps": "PT1S"}
	}}, "orders": {"columns": {"id": {"type": "bigint", "primary": true}}}}}`)
	c.Assert(err, qt.IsNil)
	c.Assert(declared(c, request), qt.DeepEquals, map[string][]changefeeds.Feed{"items": {
		feed(ydbdecl.Changefeed{Name: "keys", Mode: "KEYS_ONLY", Format: "JSON", VirtualTimestamps: true, ResolvedTimestamps: "PT1S"}),
		feed(ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "PT12H", InitialScan: true, TopicMinActivePartitions: 2,
			Consumers: []ydbdecl.Consumer{{Name: "audit", SupportedCodecs: []string{"raw", "gzip"}, ReadFrom: "2026-01-01T00:00:00Z"}, {Name: "billing", Important: true}}}),
	}})
}

func TestDecodeYAML_FailurePath(t *testing.T) {
	tests := []struct {
		name        string
		changefeeds string
		wantErr     string
	}{
		{name: "an unknown key", changefeeds: `{"f": {"mode": "UPDATES", "format": "JSON", "barriers": "PT1S"}}`,
			wantErr: `table "items": changefeed "f": unknown key "barriers"`},
		{name: "no mode", changefeeds: `{"f": {"format": "JSON"}}`,
			wantErr: `table "items": changefeed "f": invalid mode "": takes one of KEYS_ONLY, UPDATES, NEW_IMAGE, OLD_IMAGE, NEW_AND_OLD_IMAGES`},
		{name: "a consumer setting that is no boolean", changefeeds: `{"f": {"mode": "UPDATES", "format": "JSON", "consumers": {"c": {"important": "yes"}}}}`,
			wantErr: `table "items": changefeed "f": consumer "c": invalid important "yes": takes true or false`},
		{name: "an unknown consumer key", changefeeds: `{"f": {"mode": "UPDATES", "format": "JSON", "consumers": {"c": {"name": "d"}}}}`,
			wantErr: `table "items": changefeed "f": consumer "c": unknown key "name"`},
		{name: "a changefeed that is no mapping", changefeeds: `{"f": "UPDATES"}`, wantErr: `table "items": changefeed "f": a changefeed requires a mapping`},
		{name: "changefeeds that are no mapping", changefeeds: `["f"]`, wantErr: `table "items": changefeeds requires a mapping`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := testsource.YAMLTables(`{"tables": {"items": {"columns": {"id": {"type": "bigint", "primary": true}}, "changefeeds": ` + test.changefeeds + `}}}`)
			c.Assert(err, qt.IsNil)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.ErrorMatches, `(?s).*`+test.wantErr+`.*`)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// declaration is one Go directive written on a struct's comment.
func declaration(line int, structName, directive string, attributes map[string]string) testsource.GoDeclaration {
	start := (line - 1) * 100
	return testsource.GoDeclaration{Directive: directive, Attributes: attributes, Struct: structName, File: "entities.go", FileSize: 1000,
		Start: testsource.GoPosition{Offset: start, Line: line, Column: 1}, End: testsource.GoPosition{Offset: start + 40, Line: line, Column: 41}}
}

func goRequest(c *qt.C, declarations ...testsource.GoDeclaration) *protocol.DecodeSourceRequest {
	c.Helper()
	request, err := testsource.GoDirectives([]testsource.GoTable{{Name: "items", Struct: "Item"}, {Schema: "app", Name: "orders", Struct: "Order"}}, declarations)
	c.Assert(err, qt.IsNil)
	return request
}

// A Go file declares a changefeed on its table's struct, or on another struct
// with the table attribute, and each consumer of its topic with a directive of
// its own that names the changefeed.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c,
		declaration(2, "Item", "ptah:schema:changefeed", map[string]string{"name": "updates", "mode": "UPDATES", "format": "JSON", "retention_period": "PT6H"}),
		declaration(3, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "billing", "changefeed": "updates", "important": "true"}),
		declaration(4, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "audit", "changefeed": "updates", "supported_codecs": "raw,zstd"}),
		declaration(5, "Item", "ptah:schema:changefeed", map[string]string{"name": "keys", "mode": "keys_only", "format": "json", "table": "app.orders"}),
		declaration(6, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "search", "changefeed": "keys", "table": "orders"}),
	)
	c.Assert(declared(c, request), qt.DeepEquals, map[string][]changefeeds.Feed{
		"items": {feed(ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "PT6H",
			Consumers: []ydbdecl.Consumer{{Name: "audit", SupportedCodecs: []string{"raw", "zstd"}}, {Name: "billing", Important: true}}})},
		"orders": {feed(ydbdecl.Changefeed{Name: "keys", Mode: "KEYS_ONLY", Format: "JSON", Consumers: []ydbdecl.Consumer{{Name: "search"}}})},
	})
}

func TestDecodeGo_FailurePath(t *testing.T) {
	updates := map[string]string{"name": "updates", "mode": "UPDATES", "format": "JSON"}
	tests := []struct {
		name         string
		declarations []testsource.GoDeclaration
		wantErr      string
	}{
		{name: "a consumer of a changefeed the table does not declare",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", updates),
				declaration(3, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "c", "changefeed": "other"})},
			wantErr: `table "items" declares no changefeed "other" for consumer "c"`},
		{name: "a consumer of another table's changefeed",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", updates),
				declaration(3, "Order", "ptah:schema:changefeed:consumer", map[string]string{"name": "c", "changefeed": "updates"})},
			wantErr: `table "app.orders" declares no changefeed "updates" for consumer "c"`},
		{name: "a changefeed declared twice",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", updates), declaration(3, "Item", "ptah:schema:changefeed", updates)},
			wantErr:      `table "items" declares changefeed "updates" twice`},
		{name: "a consumer declared twice",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", updates),
				declaration(3, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "c", "changefeed": "updates"}),
				declaration(4, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "c", "changefeed": "updates"})},
			wantErr: `table "items": changefeed "updates" declares consumer "c" twice`},
		{name: "a consumer without its changefeed",
			declarations: []testsource.GoDeclaration{declaration(3, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "c"})},
			wantErr:      `ptah:schema:changefeed:consumer needs the changefeed whose topic it reads`},
		{name: "a table the file does not declare",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "table": "ghost"})},
			wantErr:      `table "ghost" is not declared in this file, and a changefeed is declared beside its table`},
		{name: "an invalid option",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", map[string]string{"name": "f", "mode": "UPDATES", "format": "dynamodb_streams_json"})},
			wantErr:      `invalid format "dynamodb_streams_json": YDB writes DYNAMODB_STREAMS_JSON only for a document table, .*`},
		{name: "an invalid consumer setting",
			declarations: []testsource.GoDeclaration{declaration(2, "Item", "ptah:schema:changefeed", updates),
				declaration(3, "Item", "ptah:schema:changefeed:consumer", map[string]string{"name": "c", "changefeed": "updates", "read_from": "yesterday"})},
			wantErr: `invalid read_from "yesterday": .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			decoded, err := backend.New().DecodeSource(c.Context(), goRequest(c, test.declarations...))
			c.Assert(err, qt.ErrorMatches, `(?s).*`+test.wantErr+`.*`)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// A column table takes no changefeed, and the source refuses one with the
// words of the host's bundled source.
func TestDecode_ColumnTableTakesNoChangefeed(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c,
		declaration(1, "Item", "ptah:schema:table", map[string]string{"store": "column"}),
		declaration(2, "Item", "ptah:schema:changefeed", map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON"}))
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `(?s).*table "items": row-table partitioning, column families and changefeeds cannot be applied to a column table.*`)
	c.Assert(decoded == nil, qt.IsTrue)
}
