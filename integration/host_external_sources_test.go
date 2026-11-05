//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// heldSource is what the server holds of an external data source: its type,
// its location and its properties.
type heldSource struct {
	SourceType, Location string
	Properties           map[string]string
}

func describeSource(c *qt.C, opened *connection.Connection, absolute string) heldSource {
	c.Helper()
	response, err := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver)).DescribeExternalDataSource(c.Context(),
		&Ydb_Table.DescribeExternalDataSourceRequest{Path: absolute})
	c.Assert(err, qt.IsNil)
	c.Assert(response.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", response.GetOperation().GetIssues()))
	var described Ydb_Table.DescribeExternalDataSourceResult
	c.Assert(response.GetOperation().GetResult().UnmarshalTo(&described), qt.IsNil)
	return heldSource{SourceType: described.GetSourceType(), Location: described.GetLocation(), Properties: described.GetProperties()}
}

// heldExternalTable is what the server holds of an external table: the data
// source it reads, its location, its column names and its content options.
type heldExternalTable struct {
	DataSource, Location string
	Columns              []string
	Content              map[string]string
}

func describeExternalTable(c *qt.C, opened *connection.Connection, absolute string) heldExternalTable {
	c.Helper()
	response, err := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver)).DescribeExternalTable(c.Context(),
		&Ydb_Table.DescribeExternalTableRequest{Path: absolute})
	c.Assert(err, qt.IsNil)
	c.Assert(response.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", response.GetOperation().GetIssues()))
	var described Ydb_Table.DescribeExternalTableResult
	c.Assert(response.GetOperation().GetResult().UnmarshalTo(&described), qt.IsNil)
	held := heldExternalTable{DataSource: described.GetDataSourcePath(), Location: described.GetLocation(), Content: described.GetContent()}
	for _, column := range described.GetColumns() {
		held.Columns = append(held.Columns, column.GetName())
	}
	return held
}

// externalLine is how a release line's declaration of a PostgreSQL source
// names its password: on a line with secrets, the path of a YDB secret the
// declaration holds beside the source, and on one without, which takes no
// path, a deprecated secret object the test makes itself.
type externalLine struct {
	name string
	// secrets says whether the line keeps secrets; the row runs only on such a
	// line, or only on one without.
	secrets bool
	// password is the option that names the password, with {dir} for the
	// fixture directory and {object} for the secret object's name.
	password string
	// declared is the YAML the declaration carries beside the sources, with
	// {dir} and {variable} for the variable the secret's value is read from.
	declared string
	// setup and teardown are run by hand, with {object} replaced.
	setup, teardown []string
}

var externalLines = []externalLine{
	{name: "a secret path", secrets: true, password: "PASSWORD_SECRET_PATH: {dir}/pg_password",
		declared: "secrets:\n  pg_password:\n    schema: {dir}\n    value_env: {variable}\n"},
	{name: "a secret object's name", password: "PASSWORD_SECRET_NAME: {object}",
		setup: []string{"CREATE OBJECT `{object}` (TYPE SECRET) WITH value = 'probe'"}, teardown: []string{"DROP OBJECT `{object}` (TYPE SECRET)"}},
}

// externalDocument declares a PostgreSQL source whose password is named as
// the line says, an object storage source in the directory src below the
// fixture, at location, and an external table over it whose options carry a
// semicolon and a partitioning, with the extra column given.
func externalDocument(line externalLine, location, extra string) string {
	return "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n" + line.declared +
		"external_data_sources:\n  warehouse:\n    schema: {dir}\n    source_type: PostgreSQL\n    location: pg.invalid:5432\n    auth_method: BASIC\n" +
		"    options:\n      DATABASE_NAME: app\n      LOGIN: reader\n      " + line.password + "\n" +
		"  bucket:\n    schema: {dir}/src\n    source_type: ObjectStorage\n    location: " + location + "\n    auth_method: NONE\n" +
		"external_tables:\n  events:\n    schema: {dir}\n    data_source: {dir}/src/bucket\n    location: 2026/\n    columns:\n" +
		"      - {name: id, type: Int64, not_null: true}\n      - {name: kind, type: Utf8}\n      - {name: amount, type: \"Decimal(22,9)\"}\n" + extra +
		"    options:\n      FORMAT: csv_with_names\n      CSV_DELIMITER: \";\"\n      PARTITIONED_BY: '[\"id\"]'\n"
}

// Ported from the host's TestYDBExternal_RoundTrip_NothingLeftToPlan, for
// what TestHostAppliesExternalObjects_HappyPath leaves out: a PostgreSQL
// source whose password is a secret's path on a line with secrets and a
// deprecated secret object's name on one without, an object storage source
// in a nested directory, and an external table over it whose options carry a
// semicolon and PARTITIONED_BY. The host creates them, finds nothing to
// change on the next two runs, and the server holds them as declared. Moved
// to another location, the bucket is created again with the external table
// over it, and nothing is left to plan.
func TestHostAppliesAPostgreSQLSourceAndANestedBucket_HappyPath(t *testing.T) {
	for _, line := range externalLines {
		t.Run(line.name, func(t *testing.T) {
			variable := secretVariable(t, "probe")
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			_, session := accessSession(c)
			map[bool]func(*qt.C, *protocol.Session, string){true: skipWithout, false: skipWith}[line.secrets](c, session, secrets.Capability)
			object := directory + "_pw"
			replacer := strings.NewReplacer("{dir}", directory, "{variable}", variable, "{object}", object)
			c.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				emptyTree(ctx, opened, path.Join(database, directory))
				dropSecretsBelow(c, opened, path.Join(database, directory))
				for _, statement := range line.teardown {
					c.Check(execFixtureSchema(ctx, opened.DB, replacer.Replace(statement)), qt.IsNil)
				}
			})
			for _, statement := range line.setup {
				c.Assert(execFixtureSchema(c.Context(), opened.DB, replacer.Replace(statement)), qt.IsNil)
			}
			const first, moved = "https://storage.invalid/events/", "https://storage.invalid/moved/"
			selectors := directory + "," + directory + "/src"

			applyUntilSynced(c, selectors, "yaml", replacer.Replace(externalDocument(line, first, "")))

			warehouse := describeSource(c, opened, path.Join(database, directory, "warehouse"))
			c.Assert([]string{warehouse.SourceType, warehouse.Location, warehouse.Properties["DATABASE_NAME"], warehouse.Properties["LOGIN"]}, qt.DeepEquals,
				[]string{"PostgreSQL", "pg.invalid:5432", "app", "reader"}, qt.Commentf("%v", warehouse.Properties))
			c.Assert(describeSource(c, opened, path.Join(database, directory, "src", "bucket")).Location, qt.Equals, first)
			events := describeExternalTable(c, opened, path.Join(database, directory, "events"))
			c.Assert(events.DataSource, qt.Equals, path.Join(database, directory, "src", "bucket"))
			c.Assert(events.Columns, qt.DeepEquals, []string{"id", "kind", "amount"})
			c.Assert([]string{events.Content["CSV_DELIMITER"], events.Content["PARTITIONED_BY"]}, qt.DeepEquals, []string{`[";"]`, `["id"]`},
				qt.Commentf("%v", events.Content))

			applyRuns(c, selectors, "yaml", replacer.Replace(externalDocument(line, moved, "")))
			c.Assert(describeSource(c, opened, path.Join(database, directory, "src", "bucket")).Location, qt.Equals, moved)
			c.Assert(describeExternalTable(c, opened, path.Join(database, directory, "events")).DataSource, qt.Equals,
				path.Join(database, directory, "src", "bucket"))
		})
	}
}

// Ported from the host's TestYDBExternal_FailurePath_RefusedWithTheFlagOff: a
// declared external data source on a server whose EnableExternalDataSources
// flag is off is refused before anything is created, by the capability it
// needs. The flag this test turns off is restored when it ends.
func TestHostRefusesExternalObjectsWithTheFlagOff_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableExternalDataSources": false})

	output, err := hostApplySource(c, directory, "yaml", strings.ReplaceAll(
		"external_data_sources:\n  bucket:\n    schema: {dir}\n    source_type: ObjectStorage\n    location: https://storage.invalid/\n    auth_method: NONE\n",
		"{dir}", directory))

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "external data source "+directory+"/bucket, which requires target capability external_data_sources, unavailable on this ydb target")
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}
