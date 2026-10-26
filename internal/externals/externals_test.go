package externals_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

func TestParseOptions_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want map[string]string
	}{
		{name: "nothing", text: "  "},
		{name: "names in any case, values as written", text: "database_name=app; Login = reader ;USE_TLS=true",
			want: map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "USE_TLS": "true"}},
		{name: "a value holding an equals sign", text: "FILE_PATTERN=a=b*.csv", want: map[string]string{"FILE_PATTERN": "a=b*.csv"}},
		{name: "a trailing separator", text: "FORMAT=json_each_row;", want: map[string]string{"FORMAT": "json_each_row"}},
		{name: "an escaped separator and backslash", text: `CSV_DELIMITER=\;;PATH=a\\b`, want: map[string]string{"CSV_DELIMITER": ";", "PATH": `a\b`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := externals.ParseOptions(test.text, externals.DataSourceReserved...)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

func TestParseOptions_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{name: "no equals sign", text: "FORMAT", want: `invalid options: "FORMAT" is not NAME=value`},
		{name: "no name", text: "=x", want: `invalid options: "=x" is not NAME=value`},
		{name: "a name with a space", text: "DATABASE NAME=x", want: `invalid options: "DATABASE NAME" is not an option name: .*`},
		{name: "a reserved name", text: "location=x", want: `invalid options: LOCATION is written with its own attribute, location`},
		{name: "the server's list", text: "REFERENCES=[]", want: `invalid options: REFERENCES lists the external tables over a data source, and the server keeps it`},
		{name: "a name written twice", text: "LOGIN=a;login=b", want: `invalid options: LOGIN is written twice`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := externals.ParseOptions(test.text, externals.DataSourceReserved...)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.IsNil)
		})
	}
}

func TestParseColumns_HappyPath(t *testing.T) {
	c := qt.New(t)
	got, err := externals.ParseColumns("id Int64 NOT NULL, `the name` Utf8, amount Decimal(22, 9) null, tags List<Utf8>")
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.DeepEquals, []ydbdecl.ExternalColumn{
		{Name: "id", Type: "Int64", NotNull: true},
		{Name: "the name", Type: "Utf8"},
		{Name: "amount", Type: "Decimal(22, 9)"},
		{Name: "tags", Type: "List<Utf8>"},
	})
}

func TestParseColumns_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{name: "nothing", text: "", want: `invalid columns: an entry is empty`},
		{name: "an empty entry", text: "id Int64,,b Utf8", want: `invalid columns: an entry is empty`},
		{name: "no type", text: "id", want: `invalid columns: column id has no type`},
		{name: "only NOT NULL", text: "id NOT NULL", want: `invalid columns: column id has no type`},
		{name: "a default the server drops", text: "id Int64 DEFAULT 1", want: `invalid columns: column id carries DEFAULT, which an external table does not take`},
		{name: "a key", text: "id Int64 PRIMARY KEY", want: `invalid columns: column id carries PRIMARY KEY, .*`},
		{name: "a family", text: "id Int64 FAMILY f", want: `invalid columns: column id carries FAMILY, .*`},
		{name: "a name written twice", text: "id Int64, id Utf8", want: `invalid columns: id is written twice`},
		{name: "an unclosed quote", text: "`id Int64", want: `invalid columns: "` + "`" + `id Int64" opens a quoted name it does not close`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := externals.ParseColumns(test.text)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.IsNil)
		})
	}
}

// postgresSource is a PostgreSQL data source whose password a YDB secret holds.
func postgresSource() ydbdecl.ExternalDataSource {
	return ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "BASIC",
		Options: map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "PASSWORD_SECRET_PATH": "ext/pg_password"}}
}

// eventsTable is an external table over files in an object storage source.
func eventsTable() ydbdecl.ExternalTable {
	return ydbdecl.ExternalTable{DataSource: "ext/s3", Location: "events/",
		Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Int64", NotNull: true}, {Name: "name", Type: "Utf8"}},
		Options: map[string]string{"FORMAT": "json_each_row", "PARTITIONED_BY": `["id"]`}}
}

func TestCheck_HappyPath(t *testing.T) {
	c := qt.New(t)
	capabilities := map[string]bool{externals.CapabilityExternalDataSources: true, externals.CapabilitySecretPaths: true}
	c.Assert(externals.CheckDataSource("ext/pg", postgresSource(), capabilities), qt.IsNil)
	c.Assert(externals.CheckTable("ext/events", eventsTable(), capabilities), qt.IsNil)
	c.Assert(externals.CheckDrop("DROP EXTERNAL TABLE ext/events", capabilities), qt.IsNil)
	c.Assert(externals.CheckReplace("external table ext/events", map[string]bool{externals.CapabilityReplace: true}), qt.IsNil)
}

func TestCheck_FailurePath(t *testing.T) {
	sources := map[string]bool{externals.CapabilityExternalDataSources: true}
	noLocation, noSource, noColumns := eventsTable(), eventsTable(), eventsTable()
	noLocation.Location, noSource.DataSource, noColumns.Columns = "", "", nil
	for _, test := range []struct {
		name string
		got  error
		want string
	}{
		{name: "a data source on a line with the flag off", got: externals.CheckDataSource("ext/pg", postgresSource(), map[string]bool{}),
			want: "external data source ext/pg, which requires target capability external_data_sources, unavailable on this ydb target"},
		{name: "a secret path on a line that reads it as a name", got: externals.CheckDataSource("ext/pg", postgresSource(), sources),
			want: "external data source ext/pg option PASSWORD_SECRET_PATH, which requires target capability external_data_source_secret_paths, unavailable on this ydb target"},
		{name: "no source type", got: externals.CheckDataSource("ext/pg", ydbdecl.ExternalDataSource{AuthMethod: "NONE"}, sources),
			want: "external data source ext/pg: it needs a source_type, such as ObjectStorage or PostgreSQL"},
		{name: "no auth method", got: externals.CheckDataSource("ext/pg", ydbdecl.ExternalDataSource{SourceType: "ObjectStorage"}, sources),
			want: "external data source ext/pg: it needs an auth_method, such as NONE or BASIC \\(`AUTH_METHOD requires key`\\)"},
		{name: "a table on a line with the flag off", got: externals.CheckTable("ext/events", eventsTable(), map[string]bool{}),
			want: "external table ext/events, which requires target capability external_data_sources, unavailable on this ydb target"},
		{name: "no data source", got: externals.CheckTable("ext/events", noSource, sources), want: "external table ext/events: it needs a data_source, the path of the data source it reads"},
		{name: "no location", got: externals.CheckTable("ext/events", noLocation, sources), want: "external table ext/events: it needs a location \\(`LOCATION requires key`\\)"},
		{name: "no column", got: externals.CheckTable("ext/events", noColumns, sources), want: "external table ext/events: it needs a column"},
		{name: "a drop on a line with the flag off", got: externals.CheckDrop("DROP EXTERNAL TABLE ext/events", map[string]bool{}),
			want: "DROP EXTERNAL TABLE ext/events, which requires target capability external_data_sources, unavailable on this ydb target"},
		{name: "a replacement without the replace flag", got: externals.CheckReplace("external table ext/events", sources),
			want: "external table ext/events replaced in place, which requires target capability external_object_replace, unavailable on this ydb target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(test.got, qt.ErrorMatches, test.want)
		})
	}
}

func TestSecretPaths_HappyPath(t *testing.T) {
	c := qt.New(t)
	got, err := externals.SecretPaths(map[string]string{"PASSWORD_SECRET_PATH": "/local/ext/pw", "TOKEN_SECRET_PATH": "ext/pw", "AWS_SECRET_PATH": "ext/aws", "LOGIN": "reader"}, "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.DeepEquals, []ydbdecl.Path{{Schema: "ext", Name: "aws"}, {Schema: "ext", Name: "pw"}})
}

func TestSecretPaths_FailurePath(t *testing.T) {
	c := qt.New(t)
	got, err := externals.SecretPaths(map[string]string{"PASSWORD_SECRET_PATH": "/other/ext/pw"}, "/local")
	c.Assert(err, qt.ErrorMatches, `option PASSWORD_SECRET_PATH names secret "/other/ext/pw", which is outside the database /local, so the data source cannot read it`)
	c.Assert(got, qt.IsNil)
}

func TestStatements(t *testing.T) {
	for _, test := range []struct {
		name string
		got  string
		want string
	}{
		{name: "a data source", got: externals.CreateDataSourceStatement("ext/pg", postgresSource(), externals.Create),
			want: "CREATE EXTERNAL DATA SOURCE `ext/pg` WITH (\n    SOURCE_TYPE = 'PostgreSQL',\n    LOCATION = 'pg:5432',\n" +
				"    AUTH_METHOD = 'BASIC',\n    DATABASE_NAME = 'app',\n    LOGIN = 'reader',\n    PASSWORD_SECRET_PATH = 'ext/pg_password'\n);"},
		{name: "a data source replaced, with no location", got: externals.CreateDataSourceStatement("/local/cluster",
			ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", AuthMethod: "MDB_BASIC", Options: map[string]string{"MDB_CLUSTER_ID": "c'1"}}, externals.Replace),
			want: "CREATE OR REPLACE EXTERNAL DATA SOURCE `/local/cluster` WITH (\n    SOURCE_TYPE = 'PostgreSQL',\n    AUTH_METHOD = 'MDB_BASIC',\n    MDB_CLUSTER_ID = 'c\\'1'\n);"},
		{name: "an external table", got: externals.CreateTableStatement("ext/events", eventsTable(), externals.Create),
			want: "CREATE EXTERNAL TABLE `ext/events` (\n    `id` Int64 NOT NULL,\n    `name` Utf8\n) WITH (\n    DATA_SOURCE = 'ext/s3',\n    LOCATION = 'events/',\n" +
				"    FORMAT = 'json_each_row',\n    PARTITIONED_BY = '[\"id\"]'\n);"},
		{name: "a data source dropped", got: externals.DropDataSourceStatement("ext/pg"), want: "DROP EXTERNAL DATA SOURCE `ext/pg`;"},
		{name: "an external table dropped", got: externals.DropTableStatement("ext/events"), want: "DROP EXTERNAL TABLE `ext/events`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(test.got, qt.Equals, test.want)
		})
	}
}

// The server keeps a data source's values as written, its option names in
// upper case and a secret's path as an absolute one, and adds REFERENCES.
func TestSameDataSource(t *testing.T) {
	read := func(options map[string]string, sourceType string) ydbdecl.ExternalDataSource {
		c := qt.New(t)
		properties := map[string]string{"AUTH_METHOD": "BASIC", "REFERENCES": `["/local/ext/events"]`}
		for name, value := range options {
			properties[name] = value
		}
		spec, err := externals.SourceFromObserved(&Ydb_Table.DescribeExternalDataSourceResult{SourceType: new(sourceType), Location: new("pg:5432"), Properties: properties}, "/local")
		c.Assert(err, qt.IsNil)
		return spec
	}
	for _, test := range []struct {
		name    string
		current ydbdecl.ExternalDataSource
		want    bool
	}{
		{name: "as described", current: read(map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "PASSWORD_SECRET_PATH": "/local/ext/pg_password"}, "PostgreSQL"), want: true},
		{name: "another login", current: read(map[string]string{"DATABASE_NAME": "app", "LOGIN": "writer", "PASSWORD_SECRET_PATH": "/local/ext/pg_password"}, "PostgreSQL")},
		{name: "another secret", current: read(map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "PASSWORD_SECRET_PATH": "/local/ext/other"}, "PostgreSQL")},
		{name: "an option left out", current: read(map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader"}, "PostgreSQL")},
		{name: "a source type in another case", current: read(map[string]string{"DATABASE_NAME": "app", "LOGIN": "reader", "PASSWORD_SECRET_PATH": "/local/ext/pg_password"}, "postgresql")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(externals.SameDataSource(postgresSource(), test.current, "/local"), qt.Equals, test.want)
		})
	}
}

// The server writes a table's data source as an absolute path and every
// option as a JSON array, and PARTITIONED_BY as the array it was given.
func TestSameTable(t *testing.T) {
	read := func(dataSource string, columns []*native.ColumnMeta, format string) ydbdecl.ExternalTable {
		c := qt.New(t)
		spec, err := externals.TableFromObserved(&native.DescribeExternalTableResult{SourceType: new("ObjectStorage"), DataSourcePath: new(dataSource), Location: new("events/"),
			Columns: columns, Content: map[string]string{"FORMAT": `["` + format + `"]`, "PARTITIONED_BY": `["id"]`}}, "/local")
		c.Assert(err, qt.IsNil)
		return spec
	}
	primitive := func(id Ydb.Type_PrimitiveTypeId) *Ydb.Type { return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}} }
	optional := func(id Ydb.Type_PrimitiveTypeId) *Ydb.Type {
		return &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: primitive(id)}}}
	}
	columns := []*native.ColumnMeta{{Name: "id", Type: primitive(Ydb.Type_INT64)}, {Name: "name", Type: optional(Ydb.Type_UTF8)}}
	spaced := eventsTable()
	spaced.Options = map[string]string{"FORMAT": "json_each_row", "PARTITIONED_BY": `[ "id" ]`}
	spaced.Columns[0].Type = "int64"
	for _, test := range []struct {
		name     string
		declared ydbdecl.ExternalTable
		current  ydbdecl.ExternalTable
		want     bool
	}{
		{name: "as described", declared: eventsTable(), current: read("/local/ext/s3", columns, "json_each_row"), want: true},
		{name: "written another way", declared: spaced, current: read("/local/ext/s3", columns, "json_each_row"), want: true},
		{name: "another data source", declared: eventsTable(), current: read("/local/ext/other", columns, "json_each_row")},
		{name: "a column that may be null", declared: eventsTable(), current: read("/local/ext/s3", []*native.ColumnMeta{{Name: "id", Type: optional(Ydb.Type_INT64)}, columns[1]}, "json_each_row")},
		{name: "columns in another order", declared: eventsTable(), current: read("/local/ext/s3", []*native.ColumnMeta{columns[1], columns[0]}, "json_each_row")},
		{name: "another format", declared: eventsTable(), current: read("/local/ext/s3", columns, "csv_with_names")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(externals.SameTable(test.declared, test.current, "/local"), qt.Equals, test.want)
		})
	}
}

// A declaration written as the provider's desired model reads back as the
// declaration it wrote.
func TestIntents_HappyPath(t *testing.T) {
	c := qt.New(t)
	source, err := externals.SourceIntent(postgresSource())
	c.Assert(err, qt.IsNil)
	c.Assert(source.GetProperties()["AUTH_METHOD"], qt.Equals, "BASIC")
	readSource, err := externals.SourceFromIntent(source, "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(readSource, qt.DeepEquals, postgresSource())
	table, err := externals.TableIntent(eventsTable())
	c.Assert(err, qt.IsNil)
	c.Assert(table.GetSourceType(), qt.Equals, "ObjectStorage")
	c.Assert(table.GetContent(), qt.DeepEquals, map[string]string{"FORMAT": `["json_each_row"]`, "PARTITIONED_BY": `["id"]`})
	readTable, err := externals.TableFromIntent(table, "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(readTable, qt.DeepEquals, eventsTable())
}

func TestDescribedTableOption_FailurePath(t *testing.T) {
	for _, described := range []string{`json_each_row`, `["a","b"]`, `[]`} {
		t.Run(described, func(t *testing.T) {
			c := qt.New(t)
			got, err := externals.DescribedTableOption("FORMAT", described)
			c.Assert(err, qt.ErrorMatches, `external table option FORMAT is .*, where the server writes a JSON array of one string`)
			c.Assert(got, qt.Equals, "")
		})
	}
}

func TestRelativePath(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "/local/ext/s3", want: "ext/s3"},
		{value: "/local/s3", want: "s3"},
		{value: "ext/s3", want: "ext/s3"},
		{value: "/other/ext/s3", want: "/other/ext/s3"},
		{value: "/localx/s3", want: "/localx/s3"},
	} {
		t.Run(test.value, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(externals.RelativePath(test.value, "/local"), qt.Equals, test.want)
		})
	}
}

// A data source changed in a way the tables over it cannot stay through: a
// drop, a recreation without CREATE OR REPLACE, and a new source type.
func TestDisplaces(t *testing.T) {
	bucket := ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://a/", AuthMethod: "NONE"}
	moved := bucket
	moved.Location = "https://b/"
	retyped := postgresSource()
	for _, test := range []struct {
		name             string
		desired          *ydbdecl.ExternalDataSource
		replace          bool
		displaces, again bool
	}{
		{name: "a drop", displaces: true},
		{name: "a recreation", desired: &moved, displaces: true, again: true},
		{name: "a replacement", desired: &moved, replace: true},
		{name: "a new source type", desired: &retyped, replace: true, displaces: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(externals.Displaces(&bucket, test.desired, test.replace), qt.Equals, test.displaces)
			c.Assert(externals.Recreates(&bucket, test.desired, test.replace), qt.Equals, test.again)
		})
	}
}
