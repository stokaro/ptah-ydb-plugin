//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// externalShape is what the server holds of the fixture's data source and
// external table: the bucket's location, and the table's data source, its
// columns as name and type, and its delimiter.
type externalShape struct {
	Location   string
	DataSource string
	Columns    []string
	Delimiter  string
}

// describedExternals reads the fixture's data source and external table
// back through the table service, or reports that the directory holds
// neither.
func describedExternals(c *qt.C, opened *connection.Connection, database, directory string) (externalShape, bool) {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	held := map[string]bool{}
	for _, entry := range listing.Children {
		held[entry.Name] = true
	}
	c.Assert(held["bucket"], qt.Equals, held["events"], qt.Commentf("the data source and the table come and go together: %v", held))
	if !held["bucket"] {
		return externalShape{}, false
	}
	client := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver))
	source, err := client.DescribeExternalDataSource(c.Context(), &Ydb_Table.DescribeExternalDataSourceRequest{Path: path.Join(database, directory, "bucket")})
	c.Assert(err, qt.IsNil)
	c.Assert(source.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	var sourceResult Ydb_Table.DescribeExternalDataSourceResult
	c.Assert(source.GetOperation().GetResult().UnmarshalTo(&sourceResult), qt.IsNil)
	table, err := client.DescribeExternalTable(c.Context(), &Ydb_Table.DescribeExternalTableRequest{Path: path.Join(database, directory, "events")})
	c.Assert(err, qt.IsNil)
	c.Assert(table.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	var tableResult Ydb_Table.DescribeExternalTableResult
	c.Assert(table.GetOperation().GetResult().UnmarshalTo(&tableResult), qt.IsNil)
	shape := externalShape{Location: sourceResult.GetLocation(), DataSource: tableResult.GetDataSourcePath()}
	for _, column := range tableResult.GetColumns() {
		name, nullable, err := schema.ColumnType(column.GetType())
		c.Assert(err, qt.IsNil)
		if !nullable || column.GetNotNull() {
			name += " NOT NULL"
		}
		shape.Columns = append(shape.Columns, column.GetName()+" "+name)
	}
	var delimiter []string
	c.Assert(json.Unmarshal([]byte(tableResult.GetContent()["CSV_DELIMITER"]), &delimiter), qt.IsNil)
	c.Assert(delimiter, qt.HasLen, 1)
	shape.Delimiter = delimiter[0]
	return shape, true
}

// The host plans a data source and an external table through this provider
// from each source that declares them: it creates both, finds nothing to
// change on the next run, drops and creates the table again with the data
// source it reads when only the data source moves, drops and creates the
// table alone when only its columns change, and drops both once the source
// no longer declares them, the table first. The target has
// EnableExternalDataSources and not EnableReplaceIfExistsForExternalEntities,
// so a changed object is dropped and created again.
func TestHostAppliesExternalObjects_HappyPath(t *testing.T) {
	source := func(location string) map[string]string {
		return map[string]string{
			"yaml": "external_data_sources:\n  bucket:\n    schema: {dir}\n    source_type: ObjectStorage\n    location: " + location + "\n    auth_method: NONE\n",
			"go":   "//ptah:schema:externaldatasource name=\"bucket\" schema=\"{dir}\" source_type=\"ObjectStorage\" location=\"" + location + "\" auth_method=\"NONE\"\n",
			"sql":  "CREATE EXTERNAL DATA SOURCE `{dir}/bucket` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = '" + location + "', AUTH_METHOD = 'NONE');\n",
		}
	}
	table := func(extra bool) map[string]string {
		yamlColumns := "      - {name: id, type: Int64, not_null: true}\n      - {name: kind, type: Utf8}\n      - {name: amount, type: \"Decimal(22,9)\"}\n"
		goColumns, sqlColumns := "id Int64 NOT NULL, kind Utf8, amount Decimal(22,9)", "id Int64 NOT NULL, kind Utf8, amount Decimal(22,9)"
		if extra {
			yamlColumns += "      - {name: note, type: Utf8}\n"
			goColumns += ", note Utf8"
			sqlColumns += ", note Utf8"
		}
		return map[string]string{
			"yaml": "external_tables:\n  events:\n    schema: {dir}\n    data_source: {dir}/bucket\n    location: 2026/\n    columns:\n" + yamlColumns +
				"    options:\n      FORMAT: csv_with_names\n      CSV_DELIMITER: \";\"\n",
			"go": "//ptah:schema:externaltable name=\"events\" schema=\"{dir}\" data_source=\"{dir}/bucket\" location=\"2026/\" columns=\"" + goColumns +
				"\" options=\"FORMAT=csv_with_names;CSV_DELIMITER=\\;\"\n",
			"sql": "CREATE EXTERNAL TABLE `{dir}/events` (" + sqlColumns + ") WITH (DATA_SOURCE = '{dir}/bucket', LOCATION = '2026/', FORMAT = 'csv_with_names', CSV_DELIMITER = ';');\n",
		}
	}
	document := func(format string, parts ...map[string]string) string {
		var text strings.Builder
		switch format {
		case "go":
			text.WriteString("package entities\n\n" + goItems + "\n")
		case "sql":
			text.WriteString(sqlItems)
		}
		for _, part := range parts {
			text.WriteString(part[format])
		}
		if format == "go" && len(parts) > 0 {
			text.WriteString("type Externals struct{}\n")
		}
		if format == "yaml" && len(parts) == 0 {
			text.WriteString("external_data_sources: {}\nexternal_tables: {}\n")
		}
		return text.String()
	}
	const first, moved = "https://storage.example.test/events/", "https://storage.example.test/archive/"
	for _, format := range []string{"yaml", "go", "sql"} {
		t.Run(format, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL TABLE `"+path.Join(database, directory, "events")+"`")
				_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(database, directory, "bucket")+"`")
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
			})
			apply := func(name, text, want string) string {
				c.Helper()
				output, err := hostApplySource(c, directory, format, strings.ReplaceAll(text, "{dir}", directory))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
				return output
			}
			columns := []string{"id Int64 NOT NULL", "kind Utf8", "amount Decimal(22,9)"}
			created := document(format, source(first), table(false))
			apply("creation", created, `(?s).*Schema apply completed successfully.*`)
			shape, found := describedExternals(c, opened, database, directory)
			c.Assert(found, qt.IsTrue)
			c.Assert(shape, qt.DeepEquals, externalShape{Location: first, DataSource: path.Join(database, directory, "bucket"), Columns: columns, Delimiter: ";"})
			apply("a second run", created, `(?s).*Schema is synced, no changes to be made.*`)

			movedSource := document(format, source(moved), table(false))
			output := apply("a moved data source", movedSource, `(?s).*Schema apply completed successfully.*`)
			c.Assert(output, qt.Matches, `(?s).*external-table-operation\n  [^\n]*events: DROP EXTERNAL TABLE.*external-data-source-operation\n  [^\n]*bucket: CREATE OR REPLACE.*external-table-operation\n  [^\n]*events: CREATE OR REPLACE.*`)
			shape, found = describedExternals(c, opened, database, directory)
			c.Assert(found, qt.IsTrue)
			c.Assert(shape.Location, qt.Equals, moved)
			c.Assert(shape.Columns, qt.DeepEquals, columns)
			apply("a run after the move", movedSource, `(?s).*Schema is synced, no changes to be made.*`)

			wider := document(format, source(moved), table(true))
			apply("a new column", wider, `(?s).*Schema apply completed successfully.*`)
			shape, found = describedExternals(c, opened, database, directory)
			c.Assert(found, qt.IsTrue)
			c.Assert(shape.Columns, qt.DeepEquals, append(columns, "note Utf8"))
			c.Assert(shape.Delimiter, qt.Equals, ";")
			apply("a run after the new column", wider, `(?s).*Schema is synced, no changes to be made.*`)

			apply("the removal", document(format), `(?s).*Schema apply completed successfully.*`)
			_, found = describedExternals(c, opened, database, directory)
			c.Assert(found, qt.IsFalse)
		})
	}
}
