//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests here hold the warnings the host writes for what an export leaves
// out of a YDB schema: `ptah schema export --to dbml`, and `ptah-compat
// schema inspect` with a template that writes JSON. The host names each
// family by the label this provider gives its models, so a YDB family reads
// "YDB topics" where the built-in engine at 74de0b65b wrote "topics"; the
// counts are the built-in's.

// jsonLossPrefix starts each warning a JSON inspection writes for what its
// document leaves out.
const jsonLossPrefix = "warning: JSON schema inspection leaves out "

// jsonInspect runs `ptah-compat schema inspect` over the comma-separated
// directories with the template format and returns the document and the
// warnings about what the document leaves out, in the order written.
func jsonInspect(c *qt.C, schemas, format string) (document string, warnings []string) {
	c.Helper()
	stdout, stderr, exit := runCompat(c, nil, "schema", "inspect", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", schemas, "--format", format)
	c.Assert(exit, qt.Equals, 0, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout, stderr))
	for line := range strings.SplitSeq(stderr, "\n") {
		if loss, found := strings.CutPrefix(line, jsonLossPrefix); found {
			warnings = append(warnings, loss)
		}
	}
	return stdout, warnings
}

// familyWarnings is the warnings about whole families, without those about
// a table's own properties and without the grants and roles of the
// database, whose counts depend on the server rather than on the fixture.
func familyWarnings(warnings []string) []string {
	return slices.DeleteFunc(slices.Clone(warnings), func(warning string) bool {
		return strings.Contains(warning, " from ") || strings.HasPrefix(warning, "grants (") || strings.HasPrefix(warning, "roles (")
	})
}

// jsonFormats are the templates that write the whole inspection as JSON.
var jsonFormats = []string{`{{ json . }}`, `{{ json .Realm }}`, `{{ json .Schema "  " }}`}

// Ported from the host's TestSchemaInspectJSON_ReportsOmittedYDBFamilies
// (internal/atlasreport), for the families both YDB lines serve: a JSON
// inspection of a directory holding two topics, a coordination node, an
// external data source and table, an async replication and a view, beside a
// resource pool and a classifier of the database, writes an empty document
// and warns once per family with its count, as the built-in engine did on
// 25.1.4.7 and 26.2.1.14. The database's default pool counts with the
// fixture's, and the replica table the replication made is no table of the
// document, on both engines.
func TestHostCompatJSONInspectReportsTheYDBFamiliesItLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	outside := outsideDirectory(c, opened, database, directory)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	source := path.Join(database, outside, "src")
	pool, classifier := directory+"_pool", directory+"_cls"
	rank, _ := workloadRanks(directory)
	createSchemaObject(c, opened, "CREATE TABLE `"+source+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+source+"`")
	for _, name := range []string{"events", "sink"} {
		createSchemaObject(c, opened, "CREATE TOPIC `"+absolute(name)+"`", "DROP TOPIC `"+absolute(name)+"`")
	}
	createNode(c, opened, absolute("locks"))
	createSchemaObject(c, opened, "CREATE EXTERNAL DATA SOURCE `"+absolute("bucket")+"` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.invalid/', AUTH_METHOD = 'NONE')",
		"DROP EXTERNAL DATA SOURCE `"+absolute("bucket")+"`")
	createSchemaObject(c, opened, "CREATE EXTERNAL TABLE `"+absolute("files")+"` (id Int64) WITH (DATA_SOURCE = '"+absolute("bucket")+"', LOCATION = 'f/', FORMAT = 'csv_with_names')",
		"DROP EXTERNAL TABLE `"+absolute("files")+"`")
	createSchemaObject(c, opened, "CREATE VIEW `"+absolute("recent")+"` WITH (security_invoker = TRUE) AS SELECT id FROM `"+source+"`", "DROP VIEW `"+absolute("recent")+"`")
	createSchemaObject(c, opened, "CREATE ASYNC REPLICATION `"+absolute("mirror")+"` FOR `"+source+"` AS `"+absolute("replica")+"` WITH (CONNECTION_STRING = '"+selfConnection(c, opened)+"')",
		"DROP ASYNC REPLICATION `"+absolute("mirror")+"` CASCADE")
	createSchemaObject(c, opened, "CREATE RESOURCE POOL `"+pool+"` WITH (CONCURRENT_QUERY_LIMIT = 5)", "DROP RESOURCE POOL `"+pool+"`")
	createSchemaObject(c, opened, fmt.Sprintf("CREATE RESOURCE POOL CLASSIFIER `%s` WITH (RESOURCE_POOL = '%s', RANK = %d, MEMBER_NAME = '%s')", classifier, pool, rank, workloadMember(directory)),
		"DROP RESOURCE POOL CLASSIFIER `"+classifier+"`")

	for _, format := range jsonFormats {
		t.Run(format, func(t *testing.T) {
			c := testcheck.New(t)
			document, warnings := jsonInspect(c, directory, format)

			c.Assert(strings.TrimSpace(document), qt.Equals, `{}`)
			c.Assert(familyWarnings(warnings), qt.DeepEquals, []string{
				"YDB async replications (1)",
				"YDB coordination nodes (1)",
				"YDB external data sources (1)",
				"YDB external tables (1)",
				"YDB resource pool classifiers (1)",
				"YDB resource pools (2)",
				"YDB topics (2)",
				"views (1)",
			})
		})
	}
}

// The newer families of TestHostCompatJSONInspectReportsTheYDBFamiliesItLeavesOut_HappyPath,
// which only a line with secrets, transfers and streaming queries serves: a
// secret, a transfer and a streaming query, beside the two topics the last two
// read and write. The built-in engine warned the same on 26.2.1.14.
func TestHostCompatJSONInspectReportsSecretsTransfersAndStreamingQueries_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	_, session := accessSession(c)
	skipWithout(c, session, secrets.Capability)
	skipWithout(c, session, replication.CapabilityTransfers)
	skipWithout(c, session, streaming.Capability)
	opened, directory, database := inspectionFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	target := path.Join(database, outside, "target")
	createSchemaObject(c, opened, "CREATE TABLE `"+target+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+target+"`")
	for _, name := range []string{"events", "sink"} {
		createSchemaObject(c, opened, "CREATE TOPIC `"+absolute(name)+"`", "DROP TOPIC `"+absolute(name)+"`")
	}
	createSchemaObject(c, opened, "CREATE SECRET `"+absolute("credentials")+"` WITH (value = 'probe')", "DROP SECRET `"+absolute("credentials")+"`")
	createSchemaObject(c, opened, "CREATE TRANSFER `"+absolute("copy")+"` FROM `"+absolute("events")+"` TO `"+target+"` USING ($m) -> { return []; }",
		"DROP TRANSFER `"+absolute("copy")+"`")
	createSchemaObject(c, opened, "CREATE STREAMING QUERY `"+absolute("stream")+"` WITH (RUN = FALSE) AS DO BEGIN PRAGMA TablePathPrefix = '"+path.Join(database, directory)+
		"'; INSERT INTO sink SELECT * FROM events; END DO", "DROP STREAMING QUERY `"+absolute("stream")+"`")

	document, warnings := jsonInspect(c, directory, `{{ json . }}`)

	c.Assert(strings.TrimSpace(document), qt.Equals, `{}`)
	c.Assert(familyWarnings(warnings), qt.DeepEquals, []string{
		"YDB secrets (1)",
		"YDB streaming queries (1)",
		"YDB topics (2)",
		"YDB transfers (1)",
	})
}

// fragmentTable creates the table events in directory, with a TTL and a
// commented key column, so the read carries losses of the table itself and of
// a column.
func fragmentTable(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	events := path.Join(database, directory, "events")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Int64 NOT NULL, created_at Timestamp, PRIMARY KEY (id)) WITH (TTL = Interval('P1D') ON created_at)",
		"DROP TABLE `"+events+"`")
	setStoredAttributes(c, opened, events, map[string]string{"ptah.comment.column.id": "private column comment"})
}

// Ported from the string, name and non-JSON rows of the host's
// TestSchemaInspectJSON_TemplateFragmentsOnlyReportSelectedProperties: a
// template that selects nothing a JSON document leaves out warns nothing,
// though the table it reads carries a TTL and a commented column. The
// built-in engine warned nothing for these on 25.1.4.7 and 26.2.1.14.
func TestHostCompatJSONFragmentsThatSelectNoLossWarnNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := inspectionFixture(c)
	fragmentTable(c, opened, database, directory)

	for _, test := range []struct {
		name, format, want string
	}{
		{name: "a string", format: `{{ json "plain text" }}`, want: `"plain text"`},
		{name: "a name", format: `{{ json (index .Realm.Schemas 0).Name }}`, want: `"` + directory + `"`},
		{name: "a template that writes no JSON", format: `{{ len .Realm.Schemas }}`, want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			document, warnings := jsonInspect(c, directory, test.format)

			c.Assert(strings.TrimSpace(document), qt.Equals, test.want)
			c.Assert(warnings, qt.HasLen, 0)
		})
	}
}

// Ported from the column row of the host's
// TestSchemaInspectJSON_TemplateFragmentsOnlyReportSelectedProperties: a
// fragment that writes a column warns only about that column, the comment it
// leaves out included, and not about the TTL of the table the column belongs
// to, nor with the comment's text. The column's default-only YDB properties
// add no warning, as with the built-in engine at 74de0b65b.
func TestHostCompatJSONColumnFragmentWarnsOnlyAboutTheColumn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	fragmentTable(c, opened, database, directory)
	scope := ` from column "id" of table "` + directory + `.events"`

	document, warnings := jsonInspect(c, directory, `{{ range .Realm.Schemas }}{{ range .Tables }}{{ json (index .Columns 0) }}{{ end }}{{ end }}`)

	c.Assert(strings.TrimSpace(document), qt.Equals, `{"name":"id","type":"Int64"}`)
	c.Assert(warnings, qt.DeepEquals, []string{"column comments (1)" + scope})
	c.Assert(strings.Join(warnings, "\n"), qt.Not(qt.Contains), "private")
}

// Ported from the host's TestSchemaInspectJSON_TableFragmentsKeepSchemaIdentity:
// a fragment that writes the table of the second directory writes that
// directory's table, though the first holds one of the same name, and warns
// only about it. The table has no nondefault native settings, so the provider
// has no omitted property to report (G229).
func TestHostCompatJSONTableFragmentKeepsItsDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	outside := outsideDirectory(c, opened, database, directory)
	first, second := path.Join(database, directory, "events"), path.Join(database, outside, "events")
	createSchemaObject(c, opened, "CREATE TABLE `"+first+"` (id Int64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+first+"`")
	createSchemaObject(c, opened, "CREATE TABLE `"+second+"` (id Int64 NOT NULL, kind Utf8, PRIMARY KEY (id))", "DROP TABLE `"+second+"`")
	document, warnings := jsonInspect(c, directory+","+outside, `{{ json (index (index .Realm.Schemas 1).Tables 0) }}`)

	c.Assert(strings.TrimSpace(document), qt.Equals,
		`{"name":"events","columns":[{"name":"id","type":"Int64"},{"name":"kind","type":"Utf8","null":true}],"primary_key":{"parts":[{"column":"id"}]}}`)
	c.Assert(warnings, qt.HasLen, 0)
}

// A live table's nondefault native TTL and column family remain visible as
// JSON omission warnings. Empty index and changefeed sets, default-only
// columns, and documented table defaults add no native warning.
func TestHostCompatJSONReportsOnlyNondefaultYDBProperties_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostOmissionCounts(c)
	opened, directory, database := inspectionFixture(c)
	events := path.Join(database, directory, "events")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, created_at Timestamp, payload Utf8 FAMILY cold, PRIMARY KEY (id), "+
		"FAMILY cold (COMPRESSION = 'lz4')) WITH (TTL = Interval('P1D') ON created_at)", "DROP TABLE `"+events+"`")

	document, warnings := jsonInspect(c, directory, `{{ json . }}`)
	nativeWarnings := slices.DeleteFunc(slices.Clone(warnings), func(warning string) bool { return !strings.HasPrefix(warning, "YDB ") })
	slices.Sort(nativeWarnings)

	c.Assert(document, qt.Contains, `"name":"events"`)
	c.Assert(nativeWarnings, qt.DeepEquals, []string{
		`YDB column properties (1) from column "payload" of table "` + directory + `.events"`,
		`YDB row-table properties (1) from table "` + directory + `.events"`,
	})
}

// Ported from the host's
// TestSchemaInspectJSON_CatalogDefaultPresenceSurvivesModelConversion: a
// column default the read finds, an empty string as well as a word, is a
// default the JSON document leaves out, and the inspection warns of each, as
// the built-in engine did on 25.1.4.7 and 26.2.1.14.
func TestHostCompatJSONInspectWarnsOfAnEmptyDefault_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "t")
	createSchemaObject(c, opened, "CREATE TABLE `"+table+"` (id Int64 NOT NULL, word Utf8 DEFAULT 'hello'u, empty Utf8 DEFAULT ''u, PRIMARY KEY (id))", "DROP TABLE `"+table+"`")

	_, warnings := jsonInspect(c, directory, `{{ json . }}`)

	c.Assert(warnings, qt.Contains, `column defaults (1) from column "word" of table "`+directory+`.t"`)
	c.Assert(warnings, qt.Contains, `column defaults (1) from column "empty" of table "`+directory+`.t"`)
}

// dbmlFamilies is a YAML source holding one object of each YDB family DBML
// cannot express, and two topics.
const dbmlFamilies = `resource_pools:
  batch: {}
resource_pool_classifiers:
  route:
    resource_pool: default
    rank: 100
    member_name: nobody
coordination_nodes:
  locks: {}
streaming_queries:
  stream:
    text: "SELECT 1;"
    run: false
secrets:
  credentials:
    value_env: PTAH_SECRET_CREDENTIALS
topics:
  events: {}
  audit: {}
async_replications:
  mirror:
    connection_string: grpc://h:2136/?database=/prod
    items:
      - {source: t, target: r}
transfers:
  copy:
    source: tp
    target: t
    using: "($m) -> { return []; }"
external_data_sources:
  bucket:
    source_type: ObjectStorage
    location: https://storage.invalid/
    auth_method: NONE
external_tables:
  files:
    data_source: bucket
    location: f/
    columns:
      - {name: id, type: Int64}
`

// Ported from the host's TestRender_ReportsYDBObjectsWithoutDBMLBlocks
// (internal/dbmlrender): a DBML export of a schema that holds only YDB
// objects writes no document and warns once per family with its count, the
// two topics as two, as the built-in engine at 74de0b65b did.
func TestHostDBMLExportReportsEveryYDBFamilyItLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "export", "--to", "dbml", "--schema-file", writeSource(c, "schema.yaml", dbmlFamilies),
		"--provider-config", manifest, "--provider-engine", "ydb")
	var document, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &document, &diagnostics

	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", document.String(), diagnostics.String()))
	c.Assert(document.String(), qt.Equals, "")
	c.Assert(diagnostics.String(), qt.Equals,
		"warning: DBML cannot express YDB async replications (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB coordination nodes (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB external data sources (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB external tables (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB resource pool classifiers (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB resource pools (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB secrets (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB streaming queries (1); the export leaves them out\n"+
			"warning: DBML cannot express YDB topics (2); the export leaves them out\n"+
			"warning: DBML cannot express YDB transfers (1); the export leaves them out\n")
}

// Ported from the YDB kinds of the host's
// TestAFormatThatCannotExpressAKindSaysSoAndSaysWhy/"DBML records unsupported
// object families" (internal/schemafile): DBML has no syntax for a
// changefeed, a TTL, a column family or a column table's storage, so a DBML
// document that declares the tables claims none of them, and applied over the
// tables that hold them plans nothing, as with the built-in engine at
// 74de0b65b on 25.1.4.7 and 26.2.1.14.
func TestHostDBMLKeepsWhatItCannotExpress_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	events, archive := path.Join(database, directory, "events"), path.Join(database, directory, "archive")
	createSchemaObject(c, opened, "CREATE TABLE `"+events+"` (id Int64 NOT NULL, created_at Timestamp, body Utf8 FAMILY cold, PRIMARY KEY (id), "+
		"FAMILY cold (COMPRESSION = 'lz4')) WITH (TTL = Interval('P1D') ON created_at)", "DROP TABLE `"+events+"`")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "ALTER TABLE `"+events+"` ADD CHANGEFEED updates WITH (MODE = 'UPDATES', FORMAT = 'JSON')"), qt.IsNil)
	createSchemaObject(c, opened, "CREATE TABLE `"+archive+"` (id Int64 NOT NULL, PRIMARY KEY (id)) PARTITION BY HASH (id) WITH (STORE = COLUMN)", "DROP TABLE `"+archive+"`")
	document := "Table " + directory + ".events {\n  id Int64 [pk, not null]\n  created_at Timestamp\n  body Utf8\n}\n\n" +
		"Table " + directory + ".archive {\n  id Int64 [pk, not null]\n}\n"

	c.Assert(hostPlanned(c, directory, "dbml", document), qt.HasLen, 0)
}
