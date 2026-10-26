//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/scheme"
	"google.golang.org/protobuf/proto"
)

// heldExternal is what the server holds of the fixture's data source and
// external table: the bucket's location, the table's columns, and each
// object's owner and direct access entries.
type heldExternal struct {
	Location     string
	Columns      []string
	SourceOwner  string
	SourceAccess []string
	TableOwner   string
	TableAccess  []string
}

func accessEntries(self *Ydb_Scheme.Entry) []string {
	var entries []string
	for _, entry := range self.GetPermissions() {
		for _, name := range entry.GetPermissionNames() {
			entries = append(entries, entry.GetSubject()+" "+name)
		}
	}
	slices.Sort(entries)
	return entries
}

// describeExternals reads the fixture's data source and external table back
// through the table service.
func describeExternals(c *qt.C, opened *connection.Connection, source, table string) heldExternal {
	c.Helper()
	client := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver))
	sourceResponse, err := client.DescribeExternalDataSource(c.Context(), &Ydb_Table.DescribeExternalDataSourceRequest{Path: source})
	c.Assert(err, qt.IsNil)
	var described Ydb_Table.DescribeExternalDataSourceResult
	c.Assert(sourceResponse.GetOperation().GetResult().UnmarshalTo(&described), qt.IsNil)
	tableResponse, err := client.DescribeExternalTable(c.Context(), &Ydb_Table.DescribeExternalTableRequest{Path: table})
	c.Assert(err, qt.IsNil)
	var columns Ydb_Table.DescribeExternalTableResult
	c.Assert(tableResponse.GetOperation().GetResult().UnmarshalTo(&columns), qt.IsNil)
	held := heldExternal{Location: described.GetLocation(), SourceOwner: described.GetSelf().GetOwner(), SourceAccess: accessEntries(described.GetSelf()),
		TableOwner: columns.GetSelf().GetOwner(), TableAccess: accessEntries(columns.GetSelf())}
	for _, column := range columns.GetColumns() {
		held.Columns = append(held.Columns, column.GetName())
	}
	return held
}

// capturedCoverage transfers an inspection's coverage of kinds into the
// comparison's terms, as the host does: the regions keep the database the read
// names, and the subjects keep theirs.
func capturedCoverage(coverage *protocol.ModelCoverage, kinds ...string) *protocol.ComparisonCoverage {
	result := &protocol.ComparisonCoverage{}
	part := func(value *protocol.NamespaceComponent) *protocol.ComparisonPart {
		if value == nil {
			return nil
		}
		return &protocol.ComparisonPart{Source: value.Source, Normalized: value.Normalized}
	}
	for _, kind := range coverage.Kinds {
		if !slices.Contains(kinds, kind.Model.Kind) {
			continue
		}
		source := &protocol.ComparisonKnowledgeSource{Outside: proto.CloneOf(kind.Knowledge)}
		for _, region := range kind.Regions {
			source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Database: part(region.Scope.Database), Catalog: part(region.Scope.Catalog), Schema: part(region.Scope.Schema)},
				Knowledge: proto.CloneOf(region.Knowledge)})
		}
		result.Kinds = append(result.Kinds, &protocol.ComparisonKindCoverage{Model: proto.CloneOf(kind.Model), Sources: []*protocol.ComparisonKnowledgeSource{source}})
	}
	for _, subject := range coverage.Subjects {
		if slices.Contains(kinds, subject.Kind) {
			result.Subjects = append(result.Subjects, &protocol.ComparisonSubjectCoverage{Kind: subject.Kind, Subject: testplanning.Subject(subject.NormalizedSubject), Knowledge: proto.CloneOf(subject.Knowledge)})
		}
	}
	return result
}

// externalHarness drives the provider's services over the fixture's data
// source bucket and external table events the way the host does.
type externalHarness struct {
	c         *qt.C
	ctx       context.Context
	client    *providerruntime.Client
	session   *protocol.Session
	opened    *connection.Connection
	directory string
	source    string
	table     string
}

// newExternalHarness opens a provider session on the fixture database and
// drops whatever the test leaves of both objects when it ends.
func newExternalHarness(c *qt.C, ctx context.Context) *externalHarness {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	h := &externalHarness{c: c, ctx: ctx, opened: opened, directory: directory, source: path.Join(database, directory, "bucket"), table: path.Join(database, directory, "events")}
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL TABLE `"+h.table+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL DATA SOURCE `"+h.source+"`")
	})
	binary, digest := providerBinary(c)
	h.client = startProvider(c, binary, digest)
	session, err := h.client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(h.client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	h.session = session
	return h
}

// plan compares the declared objects, the data source at location and the
// table over it with columns, or none for an empty location, with what the
// directory holds and renders the changes that follow, in plan order.
func (h *externalHarness) plan(location string, columns ...string) []*protocol.Unit {
	c, ctx, client, session := h.c, h.ctx, h.client, h.session
	c.Helper()
	rules := proto.CloneOf(client.Description().Engines[0].Identifiers)
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{h.directory}})
	c.Assert(err, qt.IsNil)
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: externalcompare.ServiceID, Kinds: externalcompare.Kinds(), Capabilities: session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}, Desired: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedCoverage(snapshot.Coverage, externalcompare.Kinds()...)}}
	for _, kind := range externalcompare.Kinds() {
		request.Desired.Coverage.Kinds = append(request.Desired.Coverage.Kinds, testcomparison.State(schema.NativeDesiredModel(kind)).Coverage.Kinds...)
	}
	if location != "" {
		bucket, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: location, AuthMethod: "NONE"})
		c.Assert(err, qt.IsNil)
		table := ydbdecl.ExternalTable{DataSource: h.directory + "/bucket", Location: "2026/", Options: map[string]string{"FORMAT": "csv_with_names"}}
		for _, column := range columns {
			table.Columns = append(table.Columns, ydbdecl.ExternalColumn{Name: column, Type: "Utf8"})
		}
		events, err := externals.TableIntent(table)
		c.Assert(err, qt.IsNil)
		for i, declared := range []struct {
			kind, name string
			intent     proto.Message
		}{{schema.ExternalSourceProperties, "bucket", bucket}, {schema.ExternalTableProperties, "events", events}} {
			value, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: declared.kind, Schema: h.directory, Name: declared.name}, declared.intent)
			c.Assert(err, qt.IsNil)
			normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared-" + strconv.Itoa(i), Representation: protocol.ValueInput_DESIRED, Value: value}}})
			c.Assert(err, qt.IsNil)
			request.Desired.Records = append(request.Desired.Records, &protocol.ComparisonRecord{Kind: declared.kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
		}
	}
	for i, value := range snapshot.NativeObjects {
		if !slices.Contains(externalcompare.Kinds(), value.Kind) {
			continue
		}
		normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "read-" + strconv.Itoa(i), Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}
	compared, err := client.CompareModels(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Undecided, qt.HasLen, 0)
	if len(compared.Changes) == 0 {
		return nil
	}
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: externalplan.ServiceID, Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}}
	for _, change := range compared.Changes {
		planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
	}
	planned, err := client.PlanFeatures(ctx, planning)
	c.Assert(err, qt.IsNil)
	c.Assert(planned.Diagnostics, qt.HasLen, 0)
	rendered, err := client.Render(ctx, testplanning.RenderRequest(planned, session.Capabilities))
	c.Assert(err, qt.IsNil)
	return rendered.Units
}

// statements are the operations units carry, each as the object it names and
// the way it runs, in order.
func (h *externalHarness) statements(units []*protocol.Unit) []string {
	h.c.Helper()
	var result []string
	for _, unit := range units {
		_, operation, err := schema.ExternalOperationChange(unit.GetNative())
		h.c.Assert(err, qt.IsNil)
		result = append(result, unit.GetNative().GetOwner().GetName()+" "+operation)
	}
	return result
}

func (h *externalHarness) execute(expected protocol.Execution_Outcome, units ...*protocol.Unit) {
	c := h.c
	c.Helper()
	result, err := h.client.Execute(h.ctx, &protocol.ExecuteRequest{SessionId: h.session.Id, RequestId: "external", Units: units})
	if expected == protocol.Execution_APPLIED {
		c.Assert(err, qt.IsNil)
	} else {
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	}
	c.Assert(result.Outcome, qt.Equals, expected)
}

// grant gives the subject auditor permission on target, as another writer
// would.
func (h *externalHarness) grant(target, permission string) {
	h.c.Helper()
	h.c.Assert(h.opened.Driver.Scheme().ModifyPermissions(h.ctx, target, scheme.WithGrantPermissions(scheme.Permissions{Subject: "auditor", PermissionNames: []string{permission}})), qt.IsNil)
}

// TestExternalLifecycleSubprocess drives a data source and an external table
// over it through the provider's services the way the host does, and reads
// both back from the server after each step: a creation, a data source moved
// to another location, which drops the table, creates the data source again
// and restores the table, each with the owner and the access entries the old
// one held, a change planned before another writer changed the table, and a
// removal. The target has EnableExternalDataSources and not
// EnableReplaceIfExistsForExternalEntities.
func TestExternalLifecycleSubprocess(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	h := newExternalHarness(c, ctx)
	c.Assert(h.session.Capabilities["external_data_sources"], qt.IsTrue)
	c.Assert(h.session.Capabilities["external_object_replace"], qt.IsFalse)

	created := h.plan("https://storage.example.test/a/", "id", "kind")
	c.Assert(h.statements(created), qt.DeepEquals, []string{"bucket create", "events create"})
	h.execute(protocol.Execution_APPLIED, created...)
	held := describeExternals(c, h.opened, h.source, h.table)
	c.Assert(held.Location, qt.Equals, "https://storage.example.test/a/")
	c.Assert(held.Columns, qt.DeepEquals, []string{"id", "kind"})
	c.Assert(h.plan("https://storage.example.test/a/", "id", "kind"), qt.HasLen, 0)

	// The access entries another writer granted stay through the
	// recreation: the provider gives the new objects the old ones' entries.
	h.grant(h.source, "ydb.granular.describe_schema")
	h.grant(h.table, "ydb.granular.describe_schema")
	owner := held.SourceOwner
	c.Assert(h.plan("https://storage.example.test/a/", "id", "kind"), qt.HasLen, 0)
	moved := h.plan("https://storage.example.test/b/", "id", "kind")
	c.Assert(h.statements(moved), qt.DeepEquals, []string{"events drop", "bucket recreate", "events restore"})
	h.execute(protocol.Execution_APPLIED, moved...)
	c.Assert(describeExternals(c, h.opened, h.source, h.table), qt.DeepEquals, heldExternal{Location: "https://storage.example.test/b/", Columns: []string{"id", "kind"},
		SourceOwner: owner, SourceAccess: []string{"auditor ydb.granular.describe_schema"}, TableOwner: owner, TableAccess: []string{"auditor ydb.granular.describe_schema"}})
	c.Assert(h.plan("https://storage.example.test/b/", "id", "kind"), qt.HasLen, 0)

	// A change planned against a table another writer changed since is
	// refused before any statement runs.
	stale := h.plan("https://storage.example.test/c/", "id", "kind")
	h.grant(h.table, "ydb.granular.select_row")
	h.execute(protocol.Execution_NOT_STARTED, stale...)
	c.Assert(describeExternals(c, h.opened, h.source, h.table).Location, qt.Equals, "https://storage.example.test/b/")
	h.execute(protocol.Execution_APPLIED, h.plan("https://storage.example.test/c/", "id", "kind", "note")...)
	held = describeExternals(c, h.opened, h.source, h.table)
	c.Assert(held.Location, qt.Equals, "https://storage.example.test/c/")
	c.Assert(held.Columns, qt.DeepEquals, []string{"id", "kind", "note"})
	c.Assert(held.TableAccess, qt.DeepEquals, []string{"auditor ydb.granular.describe_schema", "auditor ydb.granular.select_row"})

	removal := h.plan("")
	c.Assert(h.statements(removal), qt.DeepEquals, []string{"events drop", "bucket drop"})
	h.execute(protocol.Execution_APPLIED, removal...)
	listing, err := h.opened.Driver.Scheme().ListDirectory(ctx, path.Join(h.opened.Database(), h.directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}

// TestExternalReplacementSubprocess turns EnableReplaceIfExistsForExternalEntities
// on through the server's dynamic configuration, and replaces a data source
// and then the external table over it in place with CREATE OR REPLACE: the
// table stays through the data source's replacement, and both keep their
// owner and access entries.
func TestExternalReplacementSubprocess(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	address, err := connection.Parse(monitoredAddress(c))
	c.Assert(err, qt.IsNil)
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableReplaceIfExistsForExternalEntities": true})
	h := newExternalHarness(c, ctx)
	c.Assert(h.session.Capabilities["external_object_replace"], qt.IsTrue)

	h.execute(protocol.Execution_APPLIED, h.plan("https://storage.example.test/a/", "id")...)
	h.grant(h.source, "ydb.granular.describe_schema")
	h.grant(h.table, "ydb.granular.describe_schema")
	owner := describeExternals(c, h.opened, h.source, h.table).SourceOwner
	moved := h.plan("https://storage.example.test/b/", "id")
	c.Assert(h.statements(moved), qt.DeepEquals, []string{"bucket replace"})
	h.execute(protocol.Execution_APPLIED, moved...)
	wider := h.plan("https://storage.example.test/b/", "id", "note")
	c.Assert(h.statements(wider), qt.DeepEquals, []string{"events replace"})
	h.execute(protocol.Execution_APPLIED, wider...)
	c.Assert(describeExternals(c, h.opened, h.source, h.table), qt.DeepEquals, heldExternal{Location: "https://storage.example.test/b/", Columns: []string{"id", "note"},
		SourceOwner: owner, SourceAccess: []string{"auditor ydb.granular.describe_schema"}, TableOwner: owner, TableAccess: []string{"auditor ydb.granular.describe_schema"}})
	c.Assert(h.plan("https://storage.example.test/b/", "id", "note"), qt.HasLen, 0)
	h.execute(protocol.Execution_APPLIED, h.plan("")...)
}
