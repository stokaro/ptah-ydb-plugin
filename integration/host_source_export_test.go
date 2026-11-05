//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"google.golang.org/protobuf/proto"
)

// The export tests read a live schema through the provider, have it write the
// read back as a source, and replan that source through the host, which must
// find nothing to change. They are the provider's half of what `ptah
// introspect` and `ptah schema inspect` do over a read: the host refuses to
// call EncodeSource for any read of this provider, because a read's coverage
// of a native kind is complete only in the directories it listed, and the
// host has no spelling for coverage a namespace narrows. So each test calls
// EncodeSource itself, with the request the host builds, and writes the
// fragments as the host's writer does: a Go struct per table with a field
// per column and the provider's directives in its comment, or a YAML table
// entry with the provider's keys. The columns are the read's common columns,
// which the host writes.

// exportHostFeatures are the features the host negotiates that a read and an
// export depend on. With common access negotiated, converting a read leaves
// out the native access requirement, since the grants travel as common
// grants.
func exportHostFeatures() []string {
	return []string{provider.PlanningLifecycleFeature, provider.DatabaseContextFeature, provider.ValueContextFeature, provider.EncodeGroupsFeature,
		provider.EntryKeysFeature, provider.HCLSourceFeature, provider.CommonObjectsFeature, provider.CommonAccessFeature}
}

// createExportFixture creates, in directory: a row table events with a TTL,
// automatic partitioning, a key Bloom filter, a column family holding one
// column and a changefeed with an important consumer; a table plain that
// states nothing; a topic stream with a consumer; and a coordination node
// workers. Everything is dropped when the test ends.
func createExportFixture(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	absolute := func(name string) string { return path.Join(database, directory, name) }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	statements := []struct{ create, drop string }{
		{"CREATE TABLE `" + absolute("events") + "` (id Uint64 NOT NULL, body Utf8, created_at Timestamp, payload String FAMILY cold, PRIMARY KEY (id), " +
			"FAMILY cold (COMPRESSION = 'lz4')) WITH (TTL = Interval('P1D') ON created_at, AUTO_PARTITIONING_BY_LOAD = ENABLED, " +
			"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, KEY_BLOOM_FILTER = ENABLED)", "DROP TABLE `" + absolute("events") + "`"},
		{"ALTER TABLE `" + absolute("events") + "` ADD CHANGEFEED updates WITH (MODE = 'UPDATES', FORMAT = 'JSON', RETENTION_PERIOD = Interval('P2D'))", ""},
		{"ALTER TOPIC `" + absolute("events/updates") + "` ADD CONSUMER audit WITH (important = TRUE)", ""},
		{"CREATE TABLE `" + absolute("plain") + "` (id Uint64 NOT NULL, label Utf8, PRIMARY KEY (id))", "DROP TABLE `" + absolute("plain") + "`"},
		{"CREATE TOPIC `" + absolute("stream") + "` (CONSUMER reader WITH (important = TRUE)) WITH (min_active_partitions = 2)", "DROP TOPIC `" + absolute("stream") + "`"},
	}
	for _, statement := range statements {
		c.Assert(execFixtureSchema(ctx, opened.DB, statement.create), qt.IsNil, qt.Commentf("%s", statement.create))
		if drop := statement.drop; drop != "" {
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				c.Check(execFixtureSchema(cleanup, opened.DB, drop), qt.IsNil)
			})
		}
	}
	c.Assert(opened.Driver.Coordination().CreateNode(ctx, absolute("workers"), coordination.NodeConfig{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 10000}), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Coordination().DropNode(cleanup, absolute("workers"))
	})
}

// readExport is a read of one directory as the host holds it before it writes
// a source: the inspected tables with their common columns, and every value
// converted to its desired form.
type readExport struct {
	client   *providerruntime.Client
	features []string
	tables   []*protocol.Table
	values   []*protocol.NormalizedValue
}

// readDirectory reads directory through the provider and converts what it
// holds to desired values.
func readDirectory(c *qt.C, directory string) readExport {
	c.Helper()
	client := startProviderFor(c, exportHostFeatures()...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	inputs, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	observed, err := client.NormalizeValues(ctx, inputs)
	c.Assert(err, qt.IsNil)
	desired, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED,
		Values: observed.Values, TargetFeatures: inputs.TargetFeatures})
	c.Assert(err, qt.IsNil)
	return readExport{client: client, features: inputs.TargetFeatures, tables: snapshot.Tables, values: desired.Values}
}

// readColumn is a common column of a read as the host's writer writes it.
type readColumn struct {
	name, ydbType     string
	nullable, primary bool
}

// columnsOf are a read table's common columns. With observed columns
// negotiated, a read reports each column as the server describes it.
func columnsOf(table *protocol.Table) []readColumn {
	var columns []readColumn
	for _, column := range table.Columns {
		observed := column.GetObserved()
		columns = append(columns, readColumn{name: observed.GetName(), ydbType: observed.GetDataType(), nullable: observed.GetIsNullable() == "YES", primary: observed.GetPrimaryKey()})
	}
	return columns
}

// structName is the Go name of a table or a column, as the host's writer
// spells it: each word capitalized, with ID in capitals.
func structName(name string) string {
	var b strings.Builder
	for word := range strings.SplitSeq(name, "_") {
		if word == "id" {
			b.WriteString("ID")
			continue
		}
		runes := []rune(word)
		b.WriteRune(unicode.ToUpper(runes[0]))
		b.WriteString(string(runes[1:]))
	}
	return b.String()
}

// exportRequest is the EncodeSource request the host builds for the read: a
// binding for each table, each table and column facet bound to its table,
// each native object on its own, and for Go every column. A binding names the
// table as the read does, in its database: the SDK binds a facet to the
// binding whose table is the facet's owner.
func exportRequest(read readExport, format protocol.SourceDocument_Format) *protocol.EncodeSourceRequest {
	request := &protocol.EncodeSourceRequest{Engine: "ydb", Format: format, TargetFeatures: read.features}
	bindings := map[string]string{}
	for i, table := range read.tables {
		id := "table-" + strconv.Itoa(i)
		bindings[table.Identity.Name] = id
		request.Bindings = append(request.Bindings, &protocol.SourceBinding{Id: id, EntryKey: table.Identity.Name, StructName: structName(table.Identity.Name),
			Object: &protocol.Reference{Engine: "ydb", Kind: "table", Database: table.Identity.Database, Schema: table.Identity.Schema, Name: table.Identity.Name}})
		for _, column := range columnsOf(table) {
			if format == protocol.SourceDocument_GO_ANNOTATIONS {
				request.Columns = append(request.Columns, &protocol.EncodeSourceColumn{BindingId: id, Name: column.name, Type: column.ydbType, Nullable: column.nullable, PrimaryKey: column.primary})
			}
		}
	}
	for i, value := range read.values {
		encoded := proto.CloneOf(value)
		encoded.Id = "value-" + strconv.Itoa(i)
		owner := value.GetValue().GetOwner()
		switch owner.GetKind() {
		case "table":
			request.Values = append(request.Values, &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: bindings[owner.GetName()], Value: encoded})
		case "column":
			request.Values = append(request.Values, &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: bindings[owner.GetPath()[0]], Value: encoded})
		default:
			request.Values = append(request.Values, &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: encoded})
		}
	}
	return request
}

// encodedParts are the parts of a reply, by the binding they are attached to,
// "" for the document's own.
func encodedParts(encoded *protocol.EncodedSource) map[string][]*protocol.SourceFragment {
	parts := map[string][]*protocol.SourceFragment{}
	for _, part := range provider.EncodedSourceParts(encoded) {
		parts[part.Fragment.BindingId] = append(parts[part.Fragment.BindingId], part.Fragment)
	}
	return parts
}

// renderGo writes the read as a Go file: a struct for each table, with the
// table directive and the provider's attributes and directives of the table
// in its comment and a field for each column with the Go type the provider
// gave it, then one struct carrying the provider's directives of the
// document.
func renderGo(c *qt.C, read readExport, request *protocol.EncodeSourceRequest, encoded *protocol.EncodedSource) string {
	c.Helper()
	types := map[string]*protocol.EncodedFieldType{}
	imports := map[string]bool{}
	for _, typed := range encoded.FieldTypes {
		types[typed.BindingId+"/"+typed.Column] = typed
		if typed.GoImport != "" {
			imports[typed.GoImport] = true
		}
	}
	parts := encodedParts(encoded)
	var b strings.Builder
	b.WriteString("package entities\n\n")
	for _, imported := range slices.Sorted(maps.Keys(imports)) {
		fmt.Fprintf(&b, "import %q\n\n", imported)
	}
	for i, table := range read.tables {
		binding := request.Bindings[i]
		attributes := fmt.Sprintf("name=%q schema=%q", table.Identity.Name, table.Identity.Schema)
		var directives []string
		for _, fragment := range parts[binding.Id] {
			if fragment.GoDirective.Directive == "ptah:schema:table" {
				attributes += goAttributes(fragment.GetValue())
				continue
			}
			directives = append(directives, "//"+fragment.GoDirective.Directive+goAttributes(fragment.GetValue())+"\n")
		}
		fmt.Fprintf(&b, "//ptah:schema:table %s\n%stype %s struct {\n", attributes, strings.Join(directives, ""), binding.StructName)
		for _, column := range columnsOf(table) {
			field := fmt.Sprintf("name=%q type=%q", column.name, column.ydbType)
			if column.primary {
				field += ` primary="true"`
			}
			if !column.nullable {
				field += ` not_null="true"`
			}
			goType := types[binding.Id+"/"+column.name]
			c.Assert(goType, qt.IsNotNil, qt.Commentf("column %s has no Go type", column.name))
			fmt.Fprintf(&b, "\t//ptah:schema:field %s\n\t%s %s\n", field, structName(column.name), goType.GoType)
		}
		b.WriteString("}\n\n")
	}
	for _, fragment := range parts[""] {
		b.WriteString("//" + fragment.GoDirective.Directive + goAttributes(fragment.GetValue()) + "\n")
	}
	b.WriteString("type Objects struct{}\n")
	return b.String()
}

// goAttributes writes a directive's attributes, each as a quoted string.
func goAttributes(value *protocol.SourceValue) string {
	var b strings.Builder
	for _, entry := range value.GetMapping().GetEntries() {
		fmt.Fprintf(&b, " %s=%q", entry.Key, entry.Value.GetText())
	}
	return b.String()
}

// renderYAML writes the read as a YAML document, in YAML's JSON form: a table
// entry for each table with its columns and the provider's keys, and the
// provider's document keys, each holding every object written under it.
func renderYAML(read readExport, request *protocol.EncodeSourceRequest, encoded *protocol.EncodedSource) string {
	parts := encodedParts(encoded)
	var tables []string
	for i, table := range read.tables {
		binding := request.Bindings[i]
		var columns []string
		for _, column := range columnsOf(table) {
			columns = append(columns, fmt.Sprintf("%q: {\"type\": %q, \"primary\": %t, \"not_null\": %t}", column.name, column.ydbType, column.primary, !column.nullable))
		}
		entries := []string{fmt.Sprintf("\"schema\": %q", table.Identity.Schema), "\"columns\": {" + strings.Join(columns, ", ") + "}"}
		platform := map[string]string{}
		for _, fragment := range parts[binding.Id] {
			key := fragment.YamlKey.Key
			if group, found := strings.CutPrefix(key, "platform."); found {
				platform[group] = yamlValue(fragment.GetValue())
				continue
			}
			entries = append(entries, fmt.Sprintf("%q: %s", key, yamlValue(fragment.GetValue())))
		}
		for _, group := range slices.Sorted(maps.Keys(platform)) {
			entries = append(entries, fmt.Sprintf("\"platform\": {%q: %s}", group, platform[group]))
		}
		tables = append(tables, fmt.Sprintf("%q: {%s}", table.Identity.Name, strings.Join(entries, ", ")))
	}
	document := []string{"\"tables\": {" + strings.Join(tables, ", ") + "}"}
	keys := map[string][]string{}
	var order []string
	for _, fragment := range parts[""] {
		key := fragment.YamlKey.Key
		if _, seen := keys[key]; !seen {
			order = append(order, key)
		}
		for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
			keys[key] = append(keys[key], fmt.Sprintf("%q: %s", entry.Key, yamlValue(entry.Value)))
		}
	}
	for _, key := range order {
		document = append(document, fmt.Sprintf("%q: {%s}", key, strings.Join(keys[key], ", ")))
	}
	return "{" + strings.Join(document, ",\n") + "}\n"
}

// yamlValue writes a source value in YAML's JSON form, keeping its kind: text
// as a string, a number and a boolean as themselves.
func yamlValue(value *protocol.SourceValue) string {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return strconv.Quote(content.Text)
	case *protocol.SourceValue_Number:
		return content.Number
	case *protocol.SourceValue_Boolean:
		return strconv.FormatBool(content.Boolean)
	case *protocol.SourceValue_Sequence:
		var items []string
		for _, item := range content.Sequence.GetValues() {
			items = append(items, yamlValue(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case *protocol.SourceValue_Mapping:
		var entries []string
		for _, entry := range content.Mapping.GetEntries() {
			entries = append(entries, fmt.Sprintf("%q: %s", entry.Key, yamlValue(entry.Value)))
		}
		return "{" + strings.Join(entries, ", ") + "}"
	default:
		return "null"
	}
}

// hostReplanSource runs the host's `schema apply --dry-run` over one source
// with this provider selected for ydb, restricted to the fixture directory,
// and returns the planned units as appliedUnits lists them. The dry run still
// opens the provider's lock node, so a test that runs it takes the lock
// fixture.
func hostReplanSource(c *qt.C, directory, format, text string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	root := c.TempDir()
	name := map[string]string{"go": "entities.go", "yaml": "schema.yaml", "hcl": "schema.hcl"}[format]
	c.Assert(os.WriteFile(filepath.Join(root, name), []byte(text), 0o600), qt.IsNil)
	source := []string{"--schema-file", filepath.Join(root, name)}
	if format == "go" {
		source = []string{"--root-dir", root}
	}
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	arguments := append([]string{"schema", "apply", "--dry-run", "--json", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory}, source...)
	cmd := exec.CommandContext(ctx, host, arguments...)
	var report, printed bytes.Buffer
	cmd.Stdout, cmd.Stderr = &report, &printed
	err := cmd.Run()
	return appliedUnits(report.Bytes(), printed.String()), err
}

// A schema read, written back as a Go file or a YAML document, and applied
// back through the host plans nothing: the provider writes every setting the
// read holds that a source declares, the host's writer adds the columns, and
// the source reads back to what the database already holds. This is the
// provider's half of the host's introspection tests
// (TestYDBIntrospect_ModelsPlanNothingAgainstTheirDatabase); the host writes
// views and comments itself, from the common wire.
func TestHostReplansAnExportedReadSubprocess(t *testing.T) {
	for _, test := range []struct {
		format protocol.SourceDocument_Format
		name   string
	}{
		{protocol.SourceDocument_GO_ANNOTATIONS, "go"},
		{protocol.SourceDocument_YAML, "yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			createExportFixture(c, opened, database, directory)
			read := readDirectory(c, directory)
			c.Assert(read.tables, qt.HasLen, 2)
			request := exportRequest(read, test.format)
			encoded, err := read.client.EncodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0, qt.Commentf("%v", encoded.Losses))
			c.Assert(encoded.Groups, qt.Not(qt.HasLen), 0)
			source := map[protocol.SourceDocument_Format]func() string{
				protocol.SourceDocument_GO_ANNOTATIONS: func() string { return renderGo(c, read, request, encoded) },
				protocol.SourceDocument_YAML:           func() string { return renderYAML(read, request, encoded) },
			}[test.format]()
			output, err := hostReplanSource(c, directory, test.name, source)
			c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
			c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", source, output))
		})
	}
}

// A Go export of a read loses the table of a nullable key column, which YDB
// takes and no declaration can ask for, and writes nothing of it; the host
// refuses a Go export with any loss, naming it. This is the provider's half of
// the host's TestYDBIntrospect_FailurePath_NullableKeyColumn.
func TestHostExportRefusesANullableKeyColumnSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "loose")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, opened.DB, "CREATE TABLE `"+table+"` (id Uint64, label Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	read := readDirectory(c, directory)
	encoded, err := read.client.EncodeSource(c.Context(), exportRequest(read, protocol.SourceDocument_GO_ANNOTATIONS))
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Groups, qt.HasLen, 0)
	c.Assert(encoded.Losses, qt.Not(qt.HasLen), 0)
	for _, loss := range encoded.Losses {
		c.Assert(loss.Message, qt.Equals, `column "id" of table "`+directory+`/loose" is a nullable key column, which no Ptah declaration can represent: `+
			`Ptah writes NOT NULL on every YDB key column`)
	}
}

// hostExportHCL runs the host's `schema export --to hcl` over a Go file with
// this provider selected for ydb, and returns the HCL document and what the
// host reported.
func hostExportHCL(c *qt.C, goSource string) (string, string) {
	c.Helper()
	host, manifest := hostManifest(c)
	root, out := c.TempDir(), filepath.Join(c.TempDir(), "schema.hcl")
	c.Assert(os.WriteFile(filepath.Join(root, "entities.go"), []byte(goSource), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "schema", "export", "--to", "hcl", "--root-dir", root, "--out", out, "--provider-config", manifest, "--provider-engine", "ydb")
	var notes bytes.Buffer
	cmd.Stdout, cmd.Stderr = &notes, &notes
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s", notes.String()))
	document, err := os.ReadFile(out)
	c.Assert(err, qt.IsNil)
	return string(document), notes.String()
}

// exportedGo declares a row table with a TTL and automatic partitioning, and
// a coordination node, in the fixture directory.
const exportedGo = "package entities\n\n" +
	"//ptah:schema:table name=\"events\" schema=\"{dir}\" auto_partitioning_by_load=\"ENABLED\" platform.ydb.row_deletion_column=\"created_at\" platform.ydb.row_deletion_interval=\"P1D\"\n" +
	"type Event struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
	"\t//ptah:schema:field name=\"created_at\" type=\"Timestamp\"\n\tCreatedAt int64\n}\n\n" +
	"//ptah:schema:coordinationnode name=\"workers\" schema=\"{dir}\" self_check_period=\"PT2S\"\ntype Workers struct{}\n"

// The host exports a Go source as HCL through this provider: the
// coordination node becomes its coordination_node block, the host warns that
// the table's YDB settings are not in the document, and the document applied
// back to the database the Go source made plans nothing, since a setting HCL
// leaves out keeps what the table holds.
func TestHostExportsAGoSourceAsHCLSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(database, directory, "workers"))
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	goSource := strings.ReplaceAll(exportedGo, "{dir}", directory)
	output, err := hostApplySource(c, directory, "go", goSource)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	document, notes := hostExportHCL(c, goSource)
	c.Assert(document, qt.Matches, `(?s).*coordination_node "workers" \{\s*schema = schema\.`+directory+`\s*self_check_period = "PT2S"\s*\}.*`, qt.Commentf("%s", document))
	c.Assert(notes, qt.Contains, "feature facet "+schema.TableProperties+" is not represented in HCL", qt.Commentf("%s\n%s", document, notes))
	output, err = hostApplySource(c, directory, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", document, output))
}
