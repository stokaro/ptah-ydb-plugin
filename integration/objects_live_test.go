//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestNativeObjectsAndNamespaceCoverageSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	child := path.Join(directory, "child")
	c.Assert(opened.Driver.Scheme().MakeDirectory(ctx, path.Join(database, child)), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(database, child)), qt.IsNil)
	})
	view := path.Join(database, directory, "v")
	source := path.Join(database, child, "bucket")
	external := path.Join(database, directory, "files")
	for _, statement := range []struct{ create, drop string }{
		{"CREATE VIEW `" + view + "` WITH (security_invoker=TRUE) AS SELECT 1 AS id", "DROP VIEW `" + view + "`"},
		{"CREATE EXTERNAL DATA SOURCE `" + source + "` WITH (SOURCE_TYPE='ObjectStorage',LOCATION='https://storage.invalid/b/',AUTH_METHOD='NONE')", "DROP EXTERNAL DATA SOURCE `" + source + "`"},
		{"CREATE EXTERNAL TABLE `" + external + "` (id Int64 NOT NULL, amount Decimal(22,9), label Utf8) WITH (DATA_SOURCE='" + source + "',LOCATION='f/',FORMAT='json_each_row')", "DROP EXTERNAL TABLE `" + external + "`"},
	} {
		_, err := opened.DB.ExecContext(ctx, statement.create)
		c.Assert(err, qt.IsNil)
		c.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := opened.DB.ExecContext(cleanup, statement.drop)
			c.Check(err, qt.IsNil)
		})
	}
	tableClient := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver))
	altered, err := tableClient.AlterTable(ctx, &Ydb_Table.AlterTableRequest{Path: view, AlterAttributes: map[string]string{"ptah.comment": "visible comment", "custom": "retained"}})
	c.Assert(err, qt.IsNil)
	c.Assert(altered.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("issues: %v", altered.GetOperation().GetIssues()))
	coordination := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	for _, node := range []struct {
		name   string
		config *Ydb_Coordination.Config
	}{
		{"plain", &Ydb_Coordination.Config{}},
		{"limits", &Ydb_Coordination.Config{SelfCheckPeriodMillis: 2500, SessionGracePeriodMillis: 9000, ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT, AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED, RateLimiterCountersMode: Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED}},
	} {
		absolute := path.Join(database, directory, node.name)
		created, err := coordination.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: absolute, Config: node.config})
		c.Assert(err, qt.IsNil)
		c.Assert(created.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
		c.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dropped, err := coordination.DropNode(cleanup, &Ydb_Coordination.DropNodeRequest{Path: absolute})
			c.Check(err, qt.IsNil)
			c.Check(dropped.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
		})
	}
	direct, err := inspect.Read(ctx, opened.Driver, database, []string{directory, child})
	c.Assert(err, qt.IsNil)
	c.Assert(direct.NativeObjects, qt.HasLen, 5)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory, child}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 0)
	c.Assert(snapshot.NativeObjects, qt.HasLen, 5)
	for _, object := range snapshot.NativeObjects {
		switch object.Kind {
		case schema.ViewProperties:
			var value native.ViewObservation
			c.Assert(protojson.Unmarshal(object.Payload, &value), qt.IsNil)
			c.Assert(value.View.QueryText, qt.Contains, "SELECT 1 AS id")
			c.Assert(value.Properties.Attributes, qt.DeepEquals, map[string]string{"ptah.comment": "visible comment", "custom": "retained"})
			c.Assert(value.View.GetSelf().GetName(), qt.Equals, "")
		case schema.CoordinationProperties:
			var value Ydb_Coordination.DescribeNodeResult
			c.Assert(protojson.Unmarshal(object.Payload, &value), qt.IsNil)
			c.Assert(value.Config.Path, qt.Equals, "")
			if object.Owner.Name == "limits" {
				c.Assert(value.Config.SelfCheckPeriodMillis, qt.Equals, uint32(2500))
				c.Assert(value.Config.SessionGracePeriodMillis, qt.Equals, uint32(9000))
				c.Assert(value.Config.ReadConsistencyMode, qt.Equals, Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT)
				c.Assert(value.Config.AttachConsistencyMode, qt.Equals, Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED)
				c.Assert(value.Config.RateLimiterCountersMode, qt.Equals, Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED)
			}
		case schema.ExternalSourceProperties:
			var value Ydb_Table.DescribeExternalDataSourceResult
			c.Assert(protojson.Unmarshal(object.Payload, &value), qt.IsNil)
			c.Assert(value.GetSourceType(), qt.Equals, "ObjectStorage")
			c.Assert(value.GetLocation(), qt.Equals, "https://storage.invalid/b/")
			c.Assert(value.Properties["AUTH_METHOD"], qt.Equals, "NONE")
			c.Assert(object.Owner.Schema, qt.Equals, child)
		case schema.ExternalTableProperties:
			var value native.DescribeExternalTableResult
			c.Assert(protojson.Unmarshal(object.Payload, &value), qt.IsNil)
			c.Assert(value.GetDataSourcePath(), qt.Equals, source)
			c.Assert(value.GetLocation(), qt.Equals, "f/")
			c.Assert(value.Columns, qt.HasLen, 3)
			c.Assert(value.Columns[0].Name, qt.Equals, "id")
			c.Assert(value.Columns[1].Name, qt.Equals, "amount")
			c.Assert(value.Columns[2].Name, qt.Equals, "label")
			typ, nullable, err := schema.ColumnType(value.Columns[1].Type)
			c.Assert(err, qt.IsNil)
			c.Assert(typ, qt.Equals, "Decimal(22,9)")
			c.Assert(nullable, qt.IsTrue)
			c.Assert(value.Content["FORMAT"], qt.Equals, `["json_each_row"]`, qt.Commentf("native content: %v", value.Content))
		default:
			c.Fatalf("unexpected native object kind %s", object.Kind)
		}
	}
	c.Assert(provider.ValidateModelCoverage(client.Description(), "ydb", snapshot.Coverage), qt.IsNil)
	assertExactObjectRegions(c, snapshot, database, []string{directory, child})
	parentOnly, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(parentOnly.NativeObjects, qt.HasLen, 4)
	assertExactObjectRegions(c, parentOnly, database, []string{directory})
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	normalized, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
	again, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, normalized), qt.IsTrue)
}

func assertExactObjectRegions(c *qt.C, snapshot *protocol.Schema, database string, schemas []string) {
	c.Helper()
	for _, record := range snapshot.Coverage.Kinds {
		c.Assert(record.Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
		if !schema.IsNativeObject(record.Model.Kind) {
			c.Assert(record.Regions, qt.HasLen, 0)
			continue
		}
		var names []string
		for _, region := range record.Regions {
			c.Assert(region.Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
			c.Assert(region.Scope.Database.Source, qt.Equals, database)
			c.Assert(region.Scope.Database.Normalized, qt.Equals, database)
			c.Assert(region.Scope.Catalog, qt.IsNotNil)
			c.Assert(region.Scope.Catalog.Source, qt.Equals, "")
			c.Assert(region.Scope.Schema, qt.IsNotNil)
			c.Assert(region.Scope.Schema.Source, qt.Equals, region.Scope.Schema.Normalized)
			names = append(names, region.Scope.Schema.Source)
		}
		c.Assert(names, qt.DeepEquals, schemas)
	}
}

func TestEmptyDirectoryHasBoundedObjectAbsence(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	snapshot, err := inspect.Read(c.Context(), opened.Driver, database, []string{directory})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.NativeObjects, qt.HasLen, 0)
	c.Assert(snapshot.Tables, qt.HasLen, 0)
	assertExactObjectRegions(c, snapshot, database, []string{directory})
}
