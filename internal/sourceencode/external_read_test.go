package sourceencode_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// readSource is a data source as a read converts it: the declaration's
// settings, and the REFERENCES YDB 26.2 lists, the external tables over the
// source.
func readSource(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	intent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://storage.example.com/bucket/", AuthMethod: "NONE"})
	c.Assert(err, qt.IsNil)
	intent.Properties["REFERENCES"] = `["/local/app/eu/events"]`
	return readValue(c, schema.ExternalSourceProperties, intent)
}

// readTable is an external table as a read converts it: its data source
// absolute, and a NOT NULL column stated by a type that is not Optional and
// no not_null flag.
func readTable(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	intent, err := externals.TableIntent(ydbdecl.ExternalTable{DataSource: "app/eu/store", Location: "/data/",
		Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Uint64", NotNull: true}, {Name: "with space", Type: "Utf8"}}, Options: map[string]string{"FORMAT": "json_each_row"}})
	c.Assert(err, qt.IsNil)
	intent.DataSourcePath = new("/local/app/eu/store")
	intent.Columns[0].NotNull = nil
	return readValue(c, schema.ExternalTableProperties, intent)
}

func readValue(c *qt.C, kind string, intent proto.Message) *protocol.NormalizedValue {
	c.Helper()
	value := normalized(c, kind, intent)
	value.Value.Owner.Database, value.NormalizedOwner.Database = "/local", "/local"
	value.Value.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
	return value
}

// fragmentText is the text of the field key of a Go directive's fragment.
func fragmentText(c *qt.C, fragment *protocol.SourceFragment, key string) string {
	c.Helper()
	for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
		if entry.GetKey() == key {
			return entry.GetValue().GetText()
		}
	}
	c.Fatalf("the fragment has no field %s", key)
	return ""
}

// A data source and an external table a read converts are written as the
// declarations the comparison reads them as, which decode to the same
// declaration: the REFERENCES a read lists over a source, and the not_null
// flag a declaration sets beside a type that is not Optional, are not
// settings a declaration states. The Go directive writes the data source
// relative to the database and a plain column name bare, as the built-in
// engine wrote them. A byte comparison refused both (`the source grammar
// cannot preserve every setting of this declaration`).
func TestExternalObjectsFromARead_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			source, table := readSource(c), readTable(c)

			sourceResult, sourceErr := sourceencode.ExternalSource(format, source)
			tableResult, tableErr := sourceencode.ExternalTable(format, table)

			c.Assert(sourceErr, qt.IsNil)
			c.Assert(sourceResult.Loss, qt.Equals, "")
			c.Assert(tableErr, qt.IsNil)
			c.Assert(tableResult.Loss, qt.Equals, "")
			decodedSource := decodeParts(c, format, sourceResult.Parts, externalsource.Decode)
			c.Assert(decodedSource, qt.HasLen, 1)
			gotSource, err := schema.DesiredExternalSource(decodedSource[0].Value.Value)
			c.Assert(err, qt.IsNil)
			sourceSpec, err := externals.SourceFromIntent(gotSource, "/local")
			c.Assert(err, qt.IsNil)
			c.Assert(sourceSpec, qt.DeepEquals, ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://storage.example.com/bucket/", AuthMethod: "NONE"})
			decodedTable := decodeParts(c, format, tableResult.Parts, externalsource.Decode)
			c.Assert(decodedTable, qt.HasLen, 1)
			gotTable, err := schema.DesiredExternalTable(decodedTable[0].Value.Value)
			c.Assert(err, qt.IsNil)
			tableSpec, err := externals.TableFromIntent(gotTable, "/local")
			c.Assert(err, qt.IsNil)
			c.Assert(tableSpec, qt.DeepEquals, ydbdecl.ExternalTable{DataSource: "app/eu/store", Location: "/data/",
				Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Uint64", NotNull: true}, {Name: "with space", Type: "Utf8"}}, Options: map[string]string{"FORMAT": "json_each_row"}})
		})
	}
}

// The Go directive of an external table writes what the built-in engine
// wrote for it: the data source relative to the database, and each plain
// column name bare, a name of other characters in backticks.
func TestExternalTableGoDirectiveFromARead(t *testing.T) {
	c := testcheck.New(t)

	result, err := sourceencode.ExternalTable(protocol.SourceDocument_GO_ANNOTATIONS, readTable(c))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Parts, qt.HasLen, 1)
	c.Assert(fragmentText(c, result.Parts[0].Fragment, "data_source"), qt.Equals, "app/eu/store")
	c.Assert(fragmentText(c, result.Parts[0].Fragment, "columns"), qt.Equals, "id Uint64 NOT NULL, `with space` Utf8")
}
