//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

func TestRowTableCreationSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "records")
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_, err := opened.DB.ExecContext(cleanup, "DROP TABLE IF EXISTS `"+absolute+"`")
		c.Check(err, qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	source := &protocol.DeclaredTable{Name: owner.Name, Schema: owner.Schema, PrimaryKey: []string{"second", "first"},
		Columns: []*protocol.DeclaredColumn{{Name: "first", Type: "Uint64", Primary: true}, {Name: "second", Type: "Utf8", Primary: true},
			{Name: "payload", Type: "String", Nullable: true}, {Name: "expires", Type: "Timestamp", Nullable: true}, {Name: "hits", Type: "Uint64"}}}
	source.Features = &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
	}
	for _, fixture := range []struct {
		model   *protocol.ValueModel
		owner   *protocol.Reference
		payload string
	}{
		{schema.TableDesiredModel(), owner, `{"primaryKey":["second","first"],"attributes":{"purpose":"native-create"},"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}],"ttlSettings":{"dateTypeColumn":{"columnName":"expires","expireAfterSeconds":3600}},"keyBloomFilter":"DISABLED","partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"4"}}`},
		{schema.ColumnDesiredModel(), &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{"records"}, Name: "payload"}, `{"family":"archive"}`},
		{schema.ColumnDesiredModel(), &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{"records"}, Name: "hits"}, `{"literal":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"7"}}}`},
	} {
		model := fixture.model
		values, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: model.Kind + "/" + fixture.owner.Name,
			Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1,
				Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: fixture.owner, Payload: []byte(fixture.payload)}}}})
		c.Assert(err, qt.IsNil)
		subject := testplanning.Subject(fixture.owner)
		if fixture.owner.Kind == "column" {
			subject.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
		}
		source.Features.Facets.Records = append(source.Features.Facets.Records, &protocol.ComparisonRecord{Kind: model.Kind, Subject: subject, Value: values.Values[0]})
	}
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
		Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE, Desired: source}}}
	prepareCapturedCreation(c, client, planning)
	projectCapturedCreation(c, client, planning)
	plan, err := client.PlanFeatures(ctx, planning)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendered, err := client.Render(ctx, testplanning.RenderRequest(plan, session.Capabilities))
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "create-table", Units: rendered.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	c.Assert(result.AppliedCount, qt.Equals, uint32(1))
	observed, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(observed.Tables, qt.HasLen, 1)
	c.Assert(observed.Tables[0].Columns, qt.HasLen, 5)
	c.Assert(observed.Tables[0].Columns[0].Type, qt.Equals, "Uint64")
	c.Assert(observed.Tables[0].Columns[2].Nullable, qt.IsTrue)
	// An already occupied path cannot turn a captured CREATE into replacement.
	_, err = opened.DB.ExecContext(ctx, "UPSERT INTO `"+absolute+"` (first,second) VALUES (1u,\"key\"u)")
	c.Assert(err, qt.IsNil)
	result, err = client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "stale-create", Units: rendered.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	var hits uint64
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT hits FROM `"+absolute+"`").Scan(&hits), qt.IsNil)
	c.Assert(hits, qt.Equals, uint64(7))
	var count uint64
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+absolute+"`").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(1))
	// Reject a bad later native unit before dispatching an otherwise valid SQL prefix.
	bad := proto.CloneOf(rendered.Units[0])
	bad.GetNative().Payload = []byte(`{"sessionId":"forged"}`)
	prefix := &protocol.Unit{ChangeId: "prefix", Body: &protocol.Unit_Sql{Sql: "DELETE FROM `" + absolute + "`"}}
	result, err = client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "invalid-batch", Units: []*protocol.Unit{prefix, bad}})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+absolute+"`").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(1))
	result, err = client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "native-transaction", TransactionId: "unused", Units: rendered.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
}

// No database session is opened: creation compilation and rendering are offline.
func TestRowTableCreationOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	features := &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
	}
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE,
			Desired: &protocol.DeclaredTable{Name: owner.Name, Schema: owner.Schema, PrimaryKey: []string{"id"},
				Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}}, Features: features}}}}
	prepareCapturedCreation(c, client, request)
	projectCapturedCreation(c, client, request)
	plan, err := client.PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendered, err := client.Render(t.Context(), testplanning.RenderRequest(plan, request.Capabilities))
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetNative().Kind, qt.Equals, tablecreate.OperationKind)
	c.Assert(rendered.Units[0].GetNative().Owner.Database, qt.Equals, "")
}

func prepareCapturedCreation(c *qt.C, client *providerruntime.Client, planning *protocol.PlanFeaturesRequest) {
	c.Helper()
	table := planning.Tables[0]
	declaration := proto.CloneOf(table.Desired)
	for _, column := range declaration.Columns {
		column.Primary = false
	}
	request := &protocol.PrepareTablesRequest{Engine: planning.Engine, Identifiers: planning.Identifiers, Capabilities: planning.Capabilities,
		Tables: []*protocol.TablePreparationInput{{Subject: table.Subject, Desired: declaration,
			CurrentKnowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_ABSENT}}}}
	original := proto.CloneOf(request)
	prepared, err := client.PrepareTables(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Complete, qt.IsTrue)
	c.Assert(prepared.Tables, qt.HasLen, 1)
	c.Assert(prepared.Tables[0].ColumnPrimaryKeysPrepared, qt.IsTrue)
	c.Assert(prepared.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, table.Desired.PrimaryKey)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
	for _, column := range declaration.Columns {
		column.Primary = slices.Contains(prepared.Tables[0].ColumnPrimaryKeys, column.Name)
	}
	table.Desired = declaration
}

func projectCapturedCreation(c *qt.C, client *providerruntime.Client, planning *protocol.PlanFeaturesRequest) {
	c.Helper()
	table := planning.Tables[0]
	request := &protocol.ProjectTableCreationsRequest{Engine: planning.Engine, Identifiers: planning.Identifiers,
		Capabilities: planning.Capabilities, TargetFeatures: planning.TargetFeatures,
		Tables: []*protocol.TableCreationInput{{Subject: table.Subject, Declaration: table.Desired,
			FacetOwners: []*protocol.ComparisonIdentity{proto.CloneOf(table.Subject)}}}}
	original := proto.CloneOf(request)
	projected, err := client.ProjectTableCreations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(projected.Complete, qt.IsTrue)
	c.Assert(projected.Tables, qt.HasLen, 1)
	c.Assert(projected.Tables[0].ColumnPrimaryKeysPrepared, qt.IsTrue)
	c.Assert(projected.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, table.Desired.PrimaryKey)
	c.Assert(projected.Tables[0].ObservedFacets, qt.HasLen, 0)
	c.Assert(projected.Tables[0].DesiredFacets, qt.HasLen, 1)
	c.Assert(proto.Equal(original, request), qt.IsTrue)
	for _, facet := range projected.Tables[0].DesiredFacets {
		observed, err := client.ConvertValues(c.Context(), &protocol.ConvertValuesRequest{
			Engine: planning.Engine, TargetFeatures: planning.TargetFeatures,
			From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED,
			Values: []*protocol.NormalizedValue{facet.Value},
		})
		c.Assert(err, qt.IsNil)
		c.Assert(observed.Values, qt.HasLen, 1)
		c.Assert(proto.Equal(observed.Values[0].Value.Owner, facet.Value.Value.Owner), qt.IsTrue)
		index := slices.IndexFunc(table.Desired.Features.Facets.Records, func(existing *protocol.ComparisonRecord) bool {
			return existing.Kind == facet.Kind && proto.Equal(existing.Subject, facet.Subject)
		})
		if index >= 0 {
			table.Desired.Features.Facets.Records[index] = proto.CloneOf(facet)
		} else {
			table.Desired.Features.Facets.Records = append(table.Desired.Features.Facets.Records, proto.CloneOf(facet))
		}
	}
}
