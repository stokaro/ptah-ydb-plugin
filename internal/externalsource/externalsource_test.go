package externalsource_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

func boolean(value bool) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: value}}
}

// mapping builds a captured mapping from entries in written order.
func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func sequence(values ...*protocol.SourceValue) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: values}}}
}

func entry(key string, value *protocol.SourceValue) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: value}
}

func models() []*protocol.ValueModel {
	return []*protocol.ValueModel{schema.NativeDesiredModel(schema.ExternalSourceProperties), schema.NativeDesiredModel(schema.ExternalTableProperties)}
}

// yamlDocument captures a YAML document's external_data_sources and
// external_tables keys, either nil for a key the document does not write.
func yamlDocument(sources, tables *protocol.SourceValue) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_YAML, Models: models()}
	for i, key := range externalsource.YAMLKeys() {
		value := []*protocol.SourceValue{sources, tables}[i]
		if value != nil {
			document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: key.Key, Selector: key.Selector, YamlKey: key, Content: &protocol.SourceFragment_Value{Value: value}})
		}
	}
	return document
}

// goDocument captures one Go file's directives, each a directive name and
// its text attributes in written order.
func goDocument(directives ...[]string) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_GO_ANNOTATIONS, Models: models()}
	for i, directive := range directives {
		var entries []*protocol.SourceEntry
		for j := 1; j+1 < len(directive); j += 2 {
			entries = append(entries, entry(directive[j], text(directive[j+1])))
		}
		claim := externalsource.GoDirectives()[0]
		if directive[0] == externalsource.DirectiveTable {
			claim = externalsource.GoDirectives()[1]
		}
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + i)), Selector: directive[0], GoDirective: claim,
			Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}})
	}
	return document
}

// declared reads what a document declares: each object's path and the host's
// specification of it, by fragment and in order.
func declared(c *qt.C, document *protocol.SourceDocument) []any {
	c.Helper()
	values, err := externalsource.Decode(c.Context(), document)
	c.Assert(err, qt.IsNil)
	var result []any
	for index := range document.Fragments {
		for _, value := range values[index] {
			owner := value.Value.Value.Owner
			display := ydbdecl.ExternalDisplay(owner.Schema, owner.Name)
			if owner.Kind == schema.ExternalSourceProperties {
				intent, err := schema.DesiredExternalSource(value.Value.Value)
				c.Assert(err, qt.IsNil)
				spec, err := externals.SourceFromIntent(intent, "/local")
				c.Assert(err, qt.IsNil)
				result = append(result, display, spec)
				continue
			}
			intent, err := schema.DesiredExternalTable(value.Value.Value)
			c.Assert(err, qt.IsNil)
			spec, err := externals.TableFromIntent(intent, "/local")
			c.Assert(err, qt.IsNil)
			result = append(result, display, spec)
		}
	}
	return result
}

var (
	bucket = ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://storage.example.test/events/", AuthMethod: "NONE"}
	pg     = ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg.example.test:5432", AuthMethod: "BASIC",
		Options: map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "PASSWORD_SECRET_PATH": "ext/pg_password"}}
	events = ydbdecl.ExternalTable{DataSource: "ext/events_bucket", Location: "2026/",
		Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Int64", NotNull: true}, {Name: "kind", Type: "Utf8"}, {Name: "amount", Type: "Decimal(22,9)"}},
		Options: map[string]string{"FORMAT": "json_each_row", "COMPRESSION": "gzip", "CSV_DELIMITER": ";"}}
)

// The host's fixture 053 declares two data sources and a table over one of
// them with Go directives; the provider reads them as the host does.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := qt.New(t)
	document := goDocument(
		[]string{externalsource.DirectiveDataSource, "name", "warehouse", "schema", "ext", "source_type", "PostgreSQL", "location", "pg.example.test:5432", "auth_method", "BASIC",
			"options", "DATABASE_NAME=app;LOGIN=reader;PASSWORD_SECRET_PATH=ext/pg_password"},
		[]string{externalsource.DirectiveDataSource, "name", "events_bucket", "schema", "/ext/", "source_type", "ObjectStorage", "location", "https://storage.example.test/events/", "auth_method", "NONE"},
		[]string{externalsource.DirectiveTable, "name", "events", "schema", "ext", "data_source", "ext/events_bucket", "location", "2026/",
			"columns", "id Int64 NOT NULL, kind Utf8, amount Decimal(22,9)", "options", `FORMAT=json_each_row;COMPRESSION=gzip;CSV_DELIMITER=\;`},
	)
	c.Assert(declared(c, document), qt.DeepEquals, []any{"ext/warehouse", pg, "ext/events_bucket", bucket, "ext/events", events})
}

// A YAML document declares the same objects under its two keys, each entry
// named by its key unless a name field names it.
func TestDecodeYAML_HappyPath(t *testing.T) {
	c := qt.New(t)
	sources := mapping(
		entry("pg", mapping(entry("name", text("warehouse")), entry("schema", text("ext")), entry("source_type", text("PostgreSQL")), entry("location", text("pg.example.test:5432")),
			entry("auth_method", text("BASIC")), entry("options", mapping(entry("database_name", text("app")), entry("LOGIN", text(" reader ")), entry("PASSWORD_SECRET_PATH", text("ext/pg_password")))))),
		entry("events_bucket", mapping(entry("schema", text("ext")), entry("source_type", text("ObjectStorage")), entry("location", text("https://storage.example.test/events/")),
			entry("auth_method", text("NONE")))),
	)
	tables := mapping(entry("events", mapping(entry("schema", text("ext")), entry("data_source", text("ext/events_bucket")), entry("location", text("2026/")),
		entry("columns", sequence(
			mapping(entry("name", text("id")), entry("type", text("Int64")), entry("not_null", boolean(true))),
			mapping(entry("name", text("kind")), entry("type", text("Utf8"))),
			mapping(entry("name", text("amount")), entry("type", text("Decimal(22,9)"))))),
		entry("options", mapping(entry("FORMAT", text("json_each_row")), entry("COMPRESSION", text("gzip")), entry("CSV_DELIMITER", text(";")))))))
	c.Assert(declared(c, yamlDocument(sources, tables)), qt.DeepEquals, []any{"ext/events_bucket", bucket, "ext/warehouse", pg, "ext/events", events})
}

// The provider decodes a document through its source service, which the SDK
// holds to the claims the description makes, and claims the kinds whole.
func TestDecodeSourceService_HappyPath(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.GoDirectives(nil, []testsource.GoDeclaration{{Directive: externalsource.DirectiveDataSource,
		Attributes: map[string]string{"name": "bucket", "source_type": "ObjectStorage", "location": "https://a/", "auth_method": "NONE"}, Struct: "Externals", File: "entities.go", FileSize: 120,
		Start: testsource.GoPosition{Offset: 10, Line: 2, Column: 1}, End: testsource.GoPosition{Offset: 90, Line: 2, Column: 81}}})
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Documents[0].Fragments[0].Values, qt.HasLen, 1)
	c.Assert(result.Documents[0].Coverage.Subjects, qt.HasLen, 1)
	c.Assert(slices.ContainsFunc(result.Documents[0].Coverage.Kinds, func(kind *protocol.ModelKindCoverage) bool {
		return kind.Model.Kind == schema.ExternalTableProperties && kind.Knowledge.State == protocol.ModelKnowledge_COMPLETE
	}), qt.IsTrue)
}

func TestDecode_FailurePath(t *testing.T) {
	source := func(fields ...*protocol.SourceEntry) *protocol.SourceValue {
		base := []*protocol.SourceEntry{entry("source_type", text("ObjectStorage")), entry("location", text("https://a/")), entry("auth_method", text("NONE"))}
		return mapping(entry("s", mapping(append(base, fields...)...)))
	}
	for _, test := range []struct {
		name     string
		document *protocol.SourceDocument
		want     string
	}{
		{name: "a data source declared twice", document: goDocument(
			[]string{externalsource.DirectiveDataSource, "name", "s", "source_type", "ObjectStorage", "auth_method", "NONE"},
			[]string{externalsource.DirectiveDataSource, "name", "s", "source_type", "ObjectStorage", "auth_method", "NONE"}),
			want: "external data source s is declared twice"},
		{name: "a name holding a slash", document: goDocument([]string{externalsource.DirectiveDataSource, "name", "ext/s", "source_type", "ObjectStorage", "auth_method", "NONE"}),
			want: `invalid name: "ext/s" holds a slash; name the directory with schema`},
		{name: "a reserved option", document: goDocument([]string{externalsource.DirectiveDataSource, "name", "s", "source_type", "ObjectStorage", "auth_method", "NONE", "options", "AUTH_METHOD=BASIC"}),
			want: "invalid options: AUTH_METHOD is written with its own attribute, auth_method"},
		{name: "no auth method", document: goDocument([]string{externalsource.DirectiveDataSource, "name", "s", "source_type", "ObjectStorage"}),
			want: "invalid auth_method: a data source needs an auth_method, such as NONE or BASIC"},
		{name: "a column with a default", document: goDocument([]string{externalsource.DirectiveTable, "name", "t", "data_source", "s", "location", "x/", "columns", "id Int64 DEFAULT 1"}),
			want: "invalid columns: column id carries DEFAULT, which an external table does not take"},
		{name: "a table without a location", document: goDocument([]string{externalsource.DirectiveTable, "name", "t", "data_source", "s", "columns", "id Int64"}),
			want: "invalid location: an external table needs a location"},
		{name: "an unknown YAML key", document: yamlDocument(source(entry("format", text("csv"))), nil), want: `external data source "s": unknown key "format"`},
		{name: "a YAML table without columns", document: yamlDocument(nil, mapping(entry("t", mapping(entry("data_source", text("s")), entry("location", text("x/")))))),
			want: `external table "t": invalid columns: an external table needs a column`},
		{name: "a YAML column that is not a mapping", document: yamlDocument(nil, mapping(entry("t", mapping(entry("data_source", text("s")), entry("location", text("x/")),
			entry("columns", sequence(text("id Int64"))))))), want: `external table "t": columns\[0\] requires a mapping`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := externalsource.Decode(c.Context(), test.document)
			c.Assert(err, qt.ErrorMatches, ".*"+test.want)
			c.Assert(values, qt.IsNil)
		})
	}
}
