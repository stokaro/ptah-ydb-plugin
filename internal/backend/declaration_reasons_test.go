package backend_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// documentKey is the YAML document key of a native object and the model the
// host enrolls for it.
type documentKey struct {
	key  *protocol.YAMLSourceKey
	kind string
}

// documentKeys are the YAML document keys of the native objects, by key.
func documentKeys() map[string]documentKey {
	keys := map[string]documentKey{topicsource.KeyTopics: {topicsource.YAMLKey(), schema.TopicProperties},
		streamingsource.KeyQueries: {streamingsource.YAMLKey(), schema.StreamingQueryProperties}}
	for _, key := range slices.Concat(externalsource.YAMLKeys(), workloadsource.YAMLKeys(), replicationsource.YAMLKeys()) {
		keys[key.Key] = documentKey{key, key.Models[0]}
	}
	return keys
}

// yamlDocument captures a YAML document written as JSON that holds the one
// document key of a native object.
func yamlDocument(c *qt.C, key, source string) *protocol.DecodeSourceRequest {
	c.Helper()
	claim, found := documentKeys()[key]
	c.Assert(found, qt.IsTrue, qt.Commentf("key %s", key))
	request, err := testsource.YAMLDocument(source, claim.key, schema.NativeDesiredModel(claim.kind))
	c.Assert(err, qt.IsNil)
	return request
}

// A native object's declaration DecodeSource refuses gives the reason a host
// prints, in the words of the host's embedded engine at 74de0b65b: the
// object by its key, the attribute and what it takes. It never quotes the
// value the declaration wrote, which the host's engine quoted (each row's
// DO_NOT_REPORT is such a value); a name it quotes is the name of a declared
// object, a column or an attribute.
func TestNativeDeclarationRefusalGivesTheReason_FailurePath(t *testing.T) {
	tests := []struct {
		name, key, source, reason string
	}{
		{name: "a topic interval", key: "topics", source: `{"topics":{"events":{"retention_period":"DO_NOT_REPORT"}}}`,
			reason: `topic "events": invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D`},
		{name: "a topic strategy", key: "topics", source: `{"topics":{"events":{"auto_partitioning_strategy":"DO_NOT_REPORT"}}}`,
			reason: `topic "events": invalid auto_partitioning_strategy: takes one of disabled, scale_up, scale_up_and_down, paused`},
		{name: "a topic codec", key: "topics", source: `{"topics":{"events":{"supported_codecs":["raw","DO_NOT_REPORT"]}}}`,
			reason: `topic "events": invalid supported_codecs: takes a comma-separated list of raw, gzip, lzop, zstd, custom; YDB keeps a list naming any other codec as no list at all`},
		{name: "a topic's partition limits", key: "topics",
			source: `{"topics":{"events":{"auto_partitioning_strategy":"scale_up","min_active_partitions":5,"max_active_partitions":2}}}`,
			reason: "topic \"events\": invalid max_active_partitions: it is below min_active_partitions, which YDB refuses (`Invalid total partition count specified`)"},
		{name: "a topic consumer", key: "topics", source: `{"topics":{"events":{"consumers":{"c":{"important":true,"availability_period":"PT7H"}}}}}`,
			reason: "topic \"events\", consumer \"c\": invalid availability_period: YDB keeps every unread record for an important consumer, so it takes no availability period as well " +
				"(`has both an important flag and a limited availability_period, which are mutually exclusive`)"},
		{name: "a topic key", key: "topics", source: `{"topics":{"events":{"retention_storage_mb":"DO_NOT_REPORT"}}}`,
			reason: `topic "events": unknown key "retention_storage_mb"`},
		{name: "a topic name", key: "topics", source: `{"topics":{"events":{"name":"a/b"}}}`,
			reason: `topic "events": invalid name "a/b": holds a slash; name the directory with schema`},
		{name: "a data source without an auth method", key: "external_data_sources",
			source: `{"external_data_sources":{"s":{"source_type":"ObjectStorage","location":"https://DO_NOT_REPORT/"}}}`,
			reason: `external data source "s": invalid auth_method: a data source needs an auth_method, such as NONE or BASIC`},
		{name: "a data source option", key: "external_data_sources",
			source: `{"external_data_sources":{"s":{"source_type":"ObjectStorage","location":"https://a/","auth_method":"NONE","options":{"AUTH_METHOD":"DO_NOT_REPORT"}}}}`,
			reason: `external data source "s": invalid options: AUTH_METHOD is written with its own attribute, auth_method`},
		{name: "a data source key", key: "external_data_sources",
			source: `{"external_data_sources":{"s":{"source_type":"ObjectStorage","location":"https://a/","auth_method":"NONE","format":"DO_NOT_REPORT"}}}`,
			reason: `external data source "s": unknown key "format"`},
		{name: "an external table without columns", key: "external_tables", source: `{"external_tables":{"t":{"data_source":"s","location":"DO_NOT_REPORT/"}}}`,
			reason: `external table "t": invalid columns: an external table needs a column`},
		{name: "an external table column", key: "external_tables",
			source: `{"external_tables":{"t":{"data_source":"s","location":"x/","columns":["id DO_NOT_REPORT"]}}}`,
			reason: `external table "t": columns[0] requires a mapping`},
		{name: "a resource pool queue", key: "resource_pools", source: `{"resource_pools":{"batch":{"queue_size":17}}}`,
			reason: "resource pool \"batch\": invalid queue_size: a queue needs concurrent_query_limit or database_load_cpu_threshold beside it " +
				"(`queue_size unsupported without concurrent_query_limit or database_load_cpu_threshold`)"},
		{name: "a resource pool percentage", key: "resource_pools", source: `{"resource_pools":{"batch":{"total_cpu_limit_percent_per_node":"DO_NOT_REPORT"}}}`,
			reason: `resource pool "batch": invalid total_cpu_limit_percent_per_node: takes a percentage from 0 to 100; leave it out for no limit`},
		{name: "a resource pool key", key: "resource_pools", source: `{"resource_pools":{"batch":{"max_queries":3}}}`,
			reason: `resource pool "batch": unknown key "max_queries"`},
		{name: "a classifier rank", key: "resource_pool_classifiers", source: `{"resource_pool_classifiers":{"c":{"resource_pool":"default","rank":"DO_NOT_REPORT"}}}`,
			reason: `resource pool classifier "c": invalid rank: takes a whole number from 0`},
		{name: "a classifier without a pool", key: "resource_pool_classifiers", source: `{"resource_pool_classifiers":{"c":{"rank":1,"member_name":"DO_NOT_REPORT"}}}`,
			reason: "resource pool classifier \"c\": invalid resource_pool: a classifier names the pool it sends queries to (`Missing required property resource_pool`)"},
		{name: "a streaming query without text", key: "streaming_queries", source: `{"streaming_queries":{"q":{"run":true}}}`,
			reason: `streaming query "q": streaming query text is required`},
		{name: "a streaming query schema statement", key: "streaming_queries", source: `{"streaming_queries":{"q":{"text":"DROP TABLE DO_NOT_REPORT;"}}}`,
			reason: `streaming query "q": streaming query text must contain data statements only`},
		{name: "a streaming query switch", key: "streaming_queries", source: `{"streaming_queries":{"q":{"text":"SELECT 1;","run":"DO_NOT_REPORT"}}}`,
			reason: `streaming query "q": run: expected a boolean`},
		{name: "a streaming query key", key: "streaming_queries", source: `{"streaming_queries":{"q":{"text":"SELECT 1;","checkpoint":"DO_NOT_REPORT"}}}`,
			reason: `streaming query "q": unknown key "checkpoint"`},
		{name: "a replication without items", key: "async_replications", source: `{"async_replications":{"mirror":{"connection_string":"grpc://DO_NOT_REPORT:2136/?database=/prod"}}}`,
			reason: `async replication "mirror": declares no item; list the tables it replicates under items`},
		{name: "a replication without a connection", key: "async_replications", source: `{"async_replications":{"mirror":{"token_secret_name":"token","items":[{"source":"a","target":"ra"}]}}}`,
			reason: "async replication \"mirror\": invalid connection_string: a replication reads another database, which YDB reaches through the connection string " +
				"(`Neither CONNECTION_STRING nor ENDPOINT/DATABASE are provided`)"},
		{name: "a replication connection with credentials", key: "async_replications",
			source: `{"async_replications":{"mirror":{"connection_string":"grpc://root:DO_NOT_REPORT@primary:2136/?database=/prod","items":[{"source":"a","target":"ra"}]}}}`,
			reason: `async replication "mirror": invalid connection_string: carries credentials, which a replication reads from a secret; name the secret with token_secret_name or user and password_secret_name`},
		{name: "a replication commit interval", key: "async_replications",
			source: `{"async_replications":{"mirror":{"connection_string":"grpc://primary:2136/?database=/prod","commit_interval":"PT7S","items":[{"source":"a","target":"ra"}]}}}`,
			reason: "async replication \"mirror\": invalid commit_interval: a commit interval belongs to the global consistency level; declare consistency_level as global " +
				"(YDB answers `Ambiguous consistency level` otherwise)"},
		{name: "a replication target", key: "async_replications",
			source: `{"async_replications":{"mirror":{"connection_string":"grpc://primary:2136/?database=/prod","items":[{"source":"a","target":"/DO_NOT_REPORT/ra"}]}}}`,
			reason: `async replication "mirror", item 1: invalid target: takes a path relative to the database root, without a leading slash, so the declaration names no database of its own`},
		{name: "a replication key", key: "async_replications",
			source: `{"async_replications":{"mirror":{"connection_string":"grpc://primary:2136/?database=/prod","ca_cert":"DO_NOT_REPORT","items":[{"source":"a","target":"ra"}]}}}`,
			reason: `async replication "mirror": field ca_cert not found`},
		{name: "a transfer flush interval", key: "transfers", source: `{"transfers":{"ingest":{"source":"tp","target":"t","using":"($m) -> { return []; }","flush_interval":"PT7.5S"}}}`,
			reason: `transfer "ingest": invalid flush_interval: YDB keeps whole seconds, and drops the fraction of this one`},
		{name: "a transfer without a lambda", key: "transfers", source: `{"transfers":{"ingest":{"source":"tp","target":"t"}}}`,
			reason: `transfer "ingest": invalid using: a transfer turns each message into rows through a lambda, such as ($msg) -> { return [<| id: $msg._offset |>]; }`},
		{name: "a transfer lambda", key: "transfers", source: `{"transfers":{"ingest":{"source":"tp","target":"t","using":"($DO_NOT_REPORT) -> { return []; };"}}}`,
			reason: `transfer "ingest": invalid using: ends with a semicolon, which would end the statement before its WITH clause`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := yamlDocument(c, test.key, test.source)

			result, err := backend.New().DecodeSource(c.Context(), request)

			c.Assert(result, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(statusReasons(c, err), qt.DeepEquals, []string{test.reason})
		})
	}
}

// A table's changefeed DecodeSource refuses gives the reason a host prints,
// in the words of the host's embedded engine, without the value the
// declaration wrote.
func TestChangefeedDeclarationRefusalGivesTheReason_FailurePath(t *testing.T) {
	tests := []struct {
		name, changefeeds, reason string
	}{
		{name: "no mode", changefeeds: `{"f":{"format":"JSON"}}`,
			reason: `table "items": changefeed "f": invalid mode: takes one of KEYS_ONLY, UPDATES, NEW_IMAGE, OLD_IMAGE, NEW_AND_OLD_IMAGES`},
		{name: "a format", changefeeds: `{"f":{"mode":"UPDATES","format":"DO_NOT_REPORT"}}`,
			reason: `table "items": changefeed "f": invalid format: takes one of JSON, DEBEZIUM_JSON`},
		{name: "an interval", changefeeds: `{"f":{"mode":"UPDATES","format":"JSON","retention_period":"PT7.5S"}}`,
			reason: `table "items": changefeed "f": invalid retention_period: the interval has a fraction of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)`},
		{name: "an interval of no length", changefeeds: `{"f":{"mode":"UPDATES","format":"JSON","resolved_timestamps":"P0D"}}`,
			reason: `table "items": changefeed "f": invalid resolved_timestamps: the interval is no time at all, and YDB takes only a positive interval`},
		{name: "a consumer switch", changefeeds: `{"f":{"mode":"UPDATES","format":"JSON","consumers":{"c":{"important":"DO_NOT_REPORT"}}}}`,
			reason: `table "items": changefeed "f": consumer "c": invalid important: takes true or false`},
		{name: "a key", changefeeds: `{"f":{"mode":"UPDATES","format":"JSON","barriers":"DO_NOT_REPORT"}}`,
			reason: `table "items": changefeed "f": unknown key "barriers"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testsource.YAMLTables(`{"tables":{"items":{"columns":{"id":{"type":"Uint64","primary":true}},"changefeeds":` + test.changefeeds + `}}}`)
			c.Assert(err, qt.IsNil)

			result, err := backend.New().DecodeSource(c.Context(), request)

			c.Assert(result, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(statusReasons(c, err), qt.DeepEquals, []string{test.reason})
		})
	}
}

// directive is one Go directive written on the struct Item.
func directive(line int, name string, attributes map[string]string) testsource.GoDeclaration {
	start := (line - 1) * 100
	return testsource.GoDeclaration{Directive: name, Attributes: attributes, Struct: "Item", File: "entities.go", FileSize: 1000,
		Start: testsource.GoPosition{Offset: start, Line: line, Column: 1}, End: testsource.GoPosition{Offset: start + 40, Line: line, Column: 41}}
}

// A native object's Go directive DecodeSource refuses gives the reason a host
// prints in front of the directive's place, in the words of the host's
// embedded engine and its annotation parser, without the value the directive
// wrote. A missing attribute the host's directive required is refused as the
// host's parser refused it.
func TestNativeDirectiveRefusalGivesTheReason_FailurePath(t *testing.T) {
	tests := []struct {
		name       string
		directives []testsource.GoDeclaration
		reason     string
	}{
		{name: "a topic interval", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:topic", map[string]string{"name": "events", "retention_period": "DO_NOT_REPORT"})},
			reason: `invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D`},
		{name: "a topic without a name", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:topic", map[string]string{"retention_period": "PT1H"})},
			reason: `missing required annotation attribute "name"`},
		{name: "a consumer of a topic the file does not declare", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:topic", map[string]string{"name": "events"}),
			directive(2, "ptah:schema:topic:consumer", map[string]string{"name": "c", "topic": "other"})},
			reason: `the file declares no topic "other" for consumer "c"`},
		{name: "a consumer without its topic", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:topic:consumer", map[string]string{"name": "c"})},
			reason: `missing required annotation attribute "topic"`},
		{name: "a changefeed without a format", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:changefeed", map[string]string{"name": "f", "mode": "UPDATES"})},
			reason: `missing required annotation attribute "format"`},
		{name: "a changefeed mode", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:changefeed", map[string]string{"name": "f", "mode": "DO_NOT_REPORT", "format": "JSON"})},
			reason: `invalid mode: takes one of KEYS_ONLY, UPDATES, NEW_IMAGE, OLD_IMAGE, NEW_AND_OLD_IMAGES`},
		{name: "a resource pool percentage", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:resourcepool", map[string]string{"name": "p", "total_cpu_limit_percent_per_node": "DO_NOT_REPORT"})},
			reason: `invalid total_cpu_limit_percent_per_node: takes a percentage from 0 to 100; leave it out for no limit`},
		{name: "a classifier without a rank", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:resourcepool:classifier", map[string]string{"name": "c", "resource_pool": "default", "member_name": "DO_NOT_REPORT"})},
			reason: `missing required annotation attribute "rank"`},
		{name: "a data source without an auth method", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:externaldatasource", map[string]string{"name": "s", "source_type": "ObjectStorage", "location": "https://DO_NOT_REPORT/"})},
			reason: `missing required annotation attribute "auth_method"`},
		{name: "a data source option", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:externaldatasource", map[string]string{"name": "s", "source_type": "ObjectStorage", "auth_method": "NONE", "options": "DO_NOT_REPORT"})},
			reason: `invalid options: an entry is not NAME=value`},
		{name: "an external table without a location", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:externaltable", map[string]string{"name": "t", "data_source": "s", "columns": "id Int64"})},
			reason: `missing required annotation attribute "location"`},
		{name: "an external table column", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:externaltable", map[string]string{"name": "t", "data_source": "s", "location": "x/", "columns": "`DO_NOT_REPORT Int64"})},
			reason: `invalid columns: an entry opens a quoted name it does not close`},
		{name: "a streaming query schema statement", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:streamingquery", map[string]string{"name": "q", "text": "DROP TABLE DO_NOT_REPORT;"})},
			reason: `streaming query "q": streaming query text must contain data statements only`},
		{name: "a streaming query without text", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:streamingquery", map[string]string{"name": "q"})},
			reason: `missing required annotation attribute "text"`},
		{name: "a replication without items", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:async_replication", map[string]string{"name": "mirror", "connection_string": "grpc://DO_NOT_REPORT:1/?database=/d"})},
			reason: `async replication "mirror" declares no item; declare the tables it replicates with //ptah:schema:async_replication:item in the same file`},
		{name: "a replication without a connection", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:async_replication", map[string]string{"name": "mirror"})},
			reason: `missing required annotation attribute "connection_string"`},
		{name: "a transfer flush interval", directives: []testsource.GoDeclaration{
			directive(1, "ptah:schema:transfer", map[string]string{"name": "ingest", "source": "tp", "target": "t", "using": "($m) -> { return []; }", "flush_interval": "PT7.5S"})},
			reason: `invalid flush_interval: YDB keeps whole seconds, and drops the fraction of this one`},
		{name: "a transfer without a lambda", directives: []testsource.GoDeclaration{directive(1, "ptah:schema:transfer", map[string]string{"name": "ingest", "source": "tp", "target": "t"})},
			reason: `missing required annotation attribute "using"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testsource.GoDirectives([]testsource.GoTable{{Name: "items", Struct: "Item"}}, test.directives)
			c.Assert(err, qt.IsNil)

			result, err := backend.New().DecodeSource(c.Context(), request)

			c.Assert(result, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(statusReasons(c, err), qt.DeepEquals, []string{test.reason})
		})
	}
}

// A refusal of a declaration's text by the YQL splitter gives no reason, and a
// host prints its code alone: the splitter's refusals can quote the text, so
// the provider passes none of them on.
func TestNativeDeclarationRefusalOfTheTextGivesNoReason_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := yamlDocument(c, "streaming_queries", `{"streaming_queries":{"q":{"text":"--!syntax_pg\nSELECT 1;"}}}`)

	result, err := backend.New().DecodeSource(c.Context(), request)

	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.HasLen, 0)
}
