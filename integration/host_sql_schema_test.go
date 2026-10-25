//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
)

// hostApplySQL runs the host's `schema apply` over a YQL schema, a file or a
// directory of files, with this provider selected for ydb, restricted to the
// fixture directory, and returns what the host printed and how it exited.
func hostApplySQL(c *qt.C, directory, schemaFile string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema-file", schemaFile, "--schemas", directory)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// writeSchema writes the documents of a YQL schema into a new directory, each
// under its name, and returns the directory.
func writeSchema(c *qt.C, documents map[string]string) string {
	c.Helper()
	root := c.TempDir()
	for name, text := range documents {
		c.Assert(os.WriteFile(filepath.Join(root, name), []byte(text), 0o600), qt.IsNil)
	}
	return root
}

// tableShape is what the server holds of a table: its key and each column
// with its type.
type tableShape struct {
	Key     []string
	Columns []string
}

func describedTable(c *qt.C, client table.Client, absolute string) tableShape {
	c.Helper()
	var shape tableShape
	err := client.Do(c.Context(), func(ctx context.Context, session table.Session) error {
		description, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		shape = tableShape{Key: description.PrimaryKey}
		for _, column := range description.Columns {
			shape.Columns = append(shape.Columns, column.Name+" "+column.Type.Yql())
		}
		return nil
	})
	c.Assert(err, qt.IsNil)
	return shape
}

// The host reads a YQL schema through this provider and creates its tables,
// a single file and a directory of files alike; the next run finds nothing to
// change, and the server holds the tables as the schema declares them.
func TestHostAppliesASQLSchema_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		documents map[string]string
		file      string
	}{
		{name: "a file", file: "schema.sql", documents: map[string]string{"schema.sql": "CREATE TABLE `{dir}/items` (id Uint64 NOT NULL, name Utf8, PRIMARY KEY (id));\n" +
			"CREATE TABLE `{dir}/orders` (region Utf8 NOT NULL, id Int64 NOT NULL, total Decimal(22,9), PRIMARY KEY (region, id));"}},
		{name: "a directory", documents: map[string]string{"01_items.sql": "CREATE TABLE `{dir}/items` (id Uint64 NOT NULL, name Utf8, PRIMARY KEY (id));",
			"02_orders.sql": "--!syntax_v1\nCREATE TABLE `{dir}/orders` (region Utf8 NOT NULL, id Int64 NOT NULL, total Decimal(22,9), PRIMARY KEY (region, id));"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				for _, name := range []string{"items", "orders"} {
					c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
				}
			})
			documents := map[string]string{}
			for name, text := range test.documents {
				documents[name] = replaceDirectory(text, directory)
			}
			root := writeSchema(c, documents)
			schemaFile := filepath.Join(root, test.file)
			for _, run := range []struct{ name, want string }{
				{"creation", `(?s).*row-table-create-operation.*Schema apply completed successfully.*`},
				{"a second run", `(?s).*No changes planned.*`},
			} {
				output, err := hostApplySQL(c, directory, schemaFile)
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
				c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
			}
			client := opened.Driver.Table()
			c.Assert(describedTable(c, client, path.Join(database, directory, "items")), qt.DeepEquals,
				tableShape{Key: []string{"id"}, Columns: []string{"id Uint64", "name Optional<Utf8>"}})
			c.Assert(describedTable(c, client, path.Join(database, directory, "orders")), qt.DeepEquals,
				tableShape{Key: []string{"region", "id"}, Columns: []string{"region Utf8", "id Int64", "total Optional<Decimal(22,9)>"}})
		})
	}
}

// The host refuses a YQL schema this provider refuses, with the provider's
// words and the position in the document, and changes nothing: a syntax
// error the host's own reader reports the same way, and a table setting a SQL
// schema reading has no home for.
func TestHostRefusesASQLSchema_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, text, want string }{
		{name: "syntax", text: "CREATE TABLE `{dir}/items` (id Uint64, PRIMARY KEY (id));",
			want: `(?s).*read SQL schema file .*schema\.sql: YQL schema: nullable primary key column "id" cannot be represented by a desired schema; declare NOT NULL \(got ";"\) at position \d+ \(line 1, column \d+\).*`},
		{name: "table settings", text: "CREATE TABLE `{dir}/items` (id Uint64 NOT NULL, ts Timestamp, PRIMARY KEY (id)) WITH (TTL = Interval('PT1H') ON ts);",
			want: "(?s).*table `[^`]+/items` declares a TTL, which a SQL schema reading has no home for.* at position 0 \\(line 1, column 1\\).*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			root := writeSchema(c, map[string]string{"schema.sql": replaceDirectory(test.text, directory)})
			output, err := hostApplySQL(c, directory, filepath.Join(root, "schema.sql"))
			c.Assert(err, qt.IsNotNil)
			c.Assert(output, qt.Matches, test.want)
			listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
			c.Assert(err, qt.IsNil)
			c.Assert(listing.Children, qt.HasLen, 0)
		})
	}
}

func replaceDirectory(text, directory string) string {
	return string(bytes.ReplaceAll([]byte(text), []byte("{dir}"), []byte(directory)))
}
