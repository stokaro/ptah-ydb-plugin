package replicationsource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// document captures one document key's mapping, written as JSON the way the
// host's YAML frontend captures it.
func document(c *qt.C, source string, key *protocol.YAMLSourceKey) *protocol.SourceDocument {
	c.Helper()
	model := schema.NativeDesiredModel(key.Models[0])
	request, err := testsource.YAMLDocument(source, key, model)
	c.Assert(err, qt.IsNil)
	return request.Documents[0]
}

// declared reads the values one decode returned for the fragment as
// directory/name keyed declarations of each kind.
func declared(c *qt.C, values []*protocol.SourceModelValue) (map[string]replication.ReplicationSpec, map[string]replication.TransferSpec) {
	c.Helper()
	replications, transfers := make(map[string]replication.ReplicationSpec), make(map[string]replication.TransferSpec)
	for _, value := range values {
		owner := value.Value.Value.Owner
		key := owner.Schema + "/" + owner.Name
		switch value.Value.Value.Kind {
		case schema.ReplicationProperties:
			spec, err := schema.DesiredReplication(value.Value.Value)
			c.Assert(err, qt.IsNil)
			replications[key] = spec
		case schema.TransferProperties:
			spec, err := schema.DesiredTransfer(value.Value.Value)
			c.Assert(err, qt.IsNil)
			transfers[key] = spec
		}
	}
	return replications, transfers
}

// TestDecode_HappyPath reads replications and transfers keyed by name, as
// the host's YAML source reads them: a name key overrides the entry's key,
// slashes around the directory are dropped, each item in the order written,
// and the consistency level as YDB keeps it.
func TestDecode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	replications := document(c, `{"async_replications": {"mirror": {
		"schema": "/replicas/", "connection_string": "grpcs://primary:2135/?database=/prod", "user": "replicator",
		"password_secret_name": "password", "consistency_level": "GLOBAL", "commit_interval": "PT30S",
		"items": [{"source": "accounts", "target": "replica/accounts"}, {"source": "/prod/ledger", "target": "replica/ledger"}]}}}`,
		replicationsource.YAMLKeys()[0])
	values, err := replicationsource.Decode(t.Context(), replications)
	c.Assert(err, qt.IsNil)
	gotReplications, _ := declared(c, values[0])
	c.Assert(gotReplications, qt.DeepEquals, map[string]replication.ReplicationSpec{"replicas/mirror": {
		Connection: replication.Connection{ConnectionString: "grpcs://primary:2135/?database=/prod", User: "replicator", PasswordSecretName: "password"},
		Items:      []replication.Item{{Source: "accounts", Target: "replica/accounts"}, {Source: "/prod/ledger", Target: "replica/ledger"}},
		// The declaration keeps the level as the host's parse lowers it.
		ConsistencyLevel: "global", CommitInterval: "PT30S",
	}})

	transfers := document(c, `{"transfers": {
		"ingest": {"source": "orders/feed", "target": "order_log", "using": "($m) -> { return []; }", "batch_size_bytes": 1048576},
		"archive": {"name": "archive_transfer", "connection_string": "grpc://primary:2136/?database=/prod", "token_secret_name": "token",
			"source": "events", "target": "archive", "using": "($m) -> { return []; }", "consumer": "archive", "flush_interval": "PT10S"}}}`,
		replicationsource.YAMLKeys()[1])
	values, err = replicationsource.Decode(t.Context(), transfers)
	c.Assert(err, qt.IsNil)
	_, gotTransfers := declared(c, values[0])
	c.Assert(gotTransfers, qt.DeepEquals, map[string]replication.TransferSpec{
		"/archive_transfer": {Connection: replication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretName: "token"},
			Source: "events", Target: "archive", Lambda: "($m) -> { return []; }", Consumer: "archive", FlushInterval: "PT10S"},
		"/ingest": {Source: "orders/feed", Target: "order_log", Lambda: "($m) -> { return []; }", BatchSizeBytes: 1048576},
	})
}

// TestDecode_FailurePath refuses what the host's YAML source refuses, with
// its words: a value YDB would refuse, an empty one, a replication with no
// item, a key neither family has, and one path declared twice.
func TestDecode_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		key     int
		wantErr string
	}{
		{name: "no item", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod"}}}`,
			wantErr: `async replication "mirror": declares no item; list the tables it replicates under items`},
		{name: "no connection", key: 0, source: `{"async_replications": {"mirror": {"token_secret_name": "token", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `async replication "mirror": invalid connection_string: a replication reads another database, .*`},
		{name: "an empty credential", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod", "token_secret_name": "", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `async replication "mirror": invalid token_secret_name: names nothing; leave the attribute out instead`},
		{name: "an absolute target", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod", "items": [{"source": "a", "target": "/prod/ra"}]}}}`,
			wantErr: `async replication "mirror", item 1: invalid target "/prod/ra": takes a path relative to .*`},
		{name: "an unknown key", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod", "ca_cert": "x", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `(?s).*field ca_cert not found.*`},
		{name: "a commit interval at the row level", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod", "commit_interval": "PT1S", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `async replication "mirror": invalid commit_interval "PT1S": a commit interval belongs to the global consistency level; .*`},
		{name: "two items at one target", key: 0, source: `{"async_replications": {"mirror": {"connection_string": "grpc://primary:2136/?database=/prod", "items": [{"source": "a", "target": "r"}, {"source": "b", "target": "r"}]}}}`,
			wantErr: `async replication "mirror": two of its items create a replica at "r", where one table can stand`},
		{name: "one path declared twice", key: 0, source: `{"async_replications": {"a": {"name": "m", "connection_string": "grpc://p:1/?database=/d", "items": [{"source": "a", "target": "ra"}]}, "b": {"name": "m", "connection_string": "grpc://p:1/?database=/d", "items": [{"source": "a", "target": "rb"}]}}}`,
			wantErr: `async replication "b": async replication m is declared twice`},
		{name: "a transfer flushing every half second", key: 1, source: `{"transfers": {"ingest": {"source": "tp", "target": "t", "using": "($m) -> { return []; }", "flush_interval": "PT0.5S"}}}`,
			wantErr: `transfer "ingest": invalid flush_interval "PT0.5S": YDB keeps whole seconds, .*`},
		{name: "a transfer with no lambda", key: 1, source: `{"transfers": {"ingest": {"source": "tp", "target": "t"}}}`,
			wantErr: `transfer "ingest": invalid using: a transfer turns each message into rows through a lambda, .*`},
		{name: "a lambda that ends the statement", key: 1, source: `{"transfers": {"ingest": {"source": "tp", "target": "t", "using": "($m) -> { return []; };"}}}`,
			wantErr: `transfer "ingest": invalid using .*: ends with a semicolon, .*`},
		// Ported from the host's TestValidateIdentity_FailurePath: a
		// replication's directory is a clean path relative to the database
		// root.
		{name: "a parent directory", key: 0, source: `{"async_replications": {"mirror": {"schema": "../app", "connection_string": "grpc://primary:2136/?database=/prod", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `async replication "mirror": invalid schema "\.\./app": is not a directory path relative to the database root`},
		{name: "an unclean directory", key: 0, source: `{"async_replications": {"mirror": {"schema": "app//etl", "connection_string": "grpc://primary:2136/?database=/prod", "items": [{"source": "a", "target": "ra"}]}}}`,
			wantErr: `async replication "mirror": invalid schema "app//etl": is not a directory path relative to the database root`},
		{name: "a name with a slash", key: 1, source: `{"transfers": {"ingest": {"name": "a/b", "source": "tp", "target": "t", "using": "($m) -> { return []; }"}}}`,
			wantErr: `transfer "ingest": invalid name "a/b": holds a slash; name the directory with schema`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			values, err := replicationsource.Decode(t.Context(), document(c, test.source, replicationsource.YAMLKeys()[test.key]))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(values, qt.IsNil)
		})
	}
}

// goDocument captures one Go file's directives, each a name and its text
// attributes, in the order written.
func goDocument(directives ...goDirective) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_GO_ANNOTATIONS,
		Models: []*protocol.ValueModel{schema.NativeDesiredModel(schema.ReplicationProperties), schema.NativeDesiredModel(schema.TransferProperties)}}
	claims := make(map[string]*protocol.GoSourceDirective)
	for _, claim := range replicationsource.GoDirectives() {
		claims[claim.Directive] = claim
	}
	for i, directive := range directives {
		var entries []*protocol.SourceEntry
		for _, pair := range directive.attributes {
			entries = append(entries, &protocol.SourceEntry{Key: pair[0], Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: pair[1]}}})
		}
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + i)), Selector: directive.name, GoDirective: claims[directive.name],
			Content: &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}}})
	}
	return document
}

type goDirective struct {
	name       string
	attributes [][2]string
}

// TestDecodeGo_HappyPath reads the directives of the host's fixture
// 050-ydb-replication: a replication with its items in the order written,
// and a transfer.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	document := goDocument(
		goDirective{replicationsource.DirectiveReplication, [][2]string{{"name", "mirror"}, {"connection_string", "grpcs://primary.example.com:2135/?database=/prod"},
			{"user", "replicator"}, {"password_secret_path", "secrets/replicator"}, {"consistency_level", "global"}, {"commit_interval", "PT30S"}}},
		goDirective{replicationsource.DirectiveItem, [][2]string{{"replication", "mirror"}, {"source", "accounts"}, {"target", "replica/accounts"}}},
		goDirective{replicationsource.DirectiveItem, [][2]string{{"replication", "mirror"}, {"source", "/prod/ledger"}, {"target", "replica/ledger"}}},
		goDirective{replicationsource.DirectiveTransfer, [][2]string{{"name", "order_transfer"}, {"source", "orders/feed"}, {"target", "order_log"},
			{"using", "($msg) -> { return [<| partition: $msg._partition |>]; }"}, {"batch_size_bytes", "1048576"}, {"flush_interval", "PT10S"}}},
	)
	values, err := replicationsource.Decode(t.Context(), document)
	c.Assert(err, qt.IsNil)
	c.Assert(values[1], qt.HasLen, 0)
	c.Assert(values[2], qt.HasLen, 0)
	replications, _ := declared(c, values[0])
	_, transfers := declared(c, values[3])
	c.Assert(replications, qt.DeepEquals, map[string]replication.ReplicationSpec{"/mirror": {
		Connection:       replication.Connection{ConnectionString: "grpcs://primary.example.com:2135/?database=/prod", User: "replicator", PasswordSecretPath: "secrets/replicator"},
		Items:            []replication.Item{{Source: "accounts", Target: "replica/accounts"}, {Source: "/prod/ledger", Target: "replica/ledger"}},
		ConsistencyLevel: "global", CommitInterval: "PT30S",
	}})
	c.Assert(transfers, qt.DeepEquals, map[string]replication.TransferSpec{"/order_transfer": {Source: "orders/feed", Target: "order_log",
		Lambda: "($msg) -> { return [<| partition: $msg._partition |>]; }", BatchSizeBytes: 1048576, FlushInterval: "PT10S"}})
}

// TestDecodeGo_FailurePath refuses with the host's words an item of a
// replication the file does not declare, two items at one target, and a
// replication with no item.
func TestDecodeGo_FailurePath(t *testing.T) {
	replicationDirective := goDirective{replicationsource.DirectiveReplication, [][2]string{{"name", "mirror"}, {"connection_string", "grpc://p:1/?database=/d"}}}
	item := func(target string) goDirective {
		return goDirective{replicationsource.DirectiveItem, [][2]string{{"replication", "mirror"}, {"source", "a"}, {"target", target}}}
	}
	tests := []struct {
		name       string
		directives []goDirective
		wantErr    string
	}{
		{name: "an item of no replication", directives: []goDirective{item("r")},
			wantErr: `the file declares no async replication "mirror" for the item replicating "a"`},
		{name: "two items at one target", directives: []goDirective{replicationDirective, item("r"), item("r")},
			wantErr: `async replication "mirror" declares two items with target "r"`},
		{name: "no item", directives: []goDirective{replicationDirective},
			wantErr: `async replication "mirror" declares no item; declare the tables it replicates with //ptah:schema:async_replication:item in the same file`},
		{name: "a transfer with no name", directives: []goDirective{{replicationsource.DirectiveTransfer, [][2]string{{"source", "tp"}, {"target", "t"}, {"using", "($m) -> { return []; }"}}}},
			wantErr: `missing required annotation attribute "name"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			values, err := replicationsource.Decode(t.Context(), goDocument(test.directives...))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(values, qt.IsNil)
		})
	}
}
