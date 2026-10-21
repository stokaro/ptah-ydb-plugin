//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func TestTableConversionOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "App", Kind: "table", Name: "Orders"}
	table, err := schema.CaptureTable(owner, &native.DescribeTableResult{PrimaryKey: []string{"id"}, Attributes: map[string]string{"comment": "offline export"}, Self: &Ydb_Scheme.Entry{Owner: "alice",
		Permissions:          []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"write"}}},
		EffectivePermissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"read"}}, {Subject: "writer", PermissionNames: []string{"write"}}}}})
	c.Assert(err, qt.IsNil)
	columnOwner := proto.CloneOf(owner)
	columnOwner.Kind = "column"
	columnOwner.Path = []string{owner.Name}
	columnOwner.Name = "id"
	column, err := schema.CaptureColumn(columnOwner, &native.ColumnMeta{Family: "default"})
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	convertProperties(c, client, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "table", Representation: protocol.ValueInput_OBSERVED, Value: table},
		{Id: "column", Representation: protocol.ValueInput_OBSERVED, Value: column},
	}})
}

func TestTableConversionLiveSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx := t.Context()
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	all, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	properties := &protocol.NormalizeValuesRequest{Engine: all.Engine, TargetFeatures: all.TargetFeatures}
	for _, value := range all.Values {
		if value.Value.Kind == schema.TableProperties || value.Value.Kind == schema.ColumnProperties {
			properties.Values = append(properties.Values, proto.CloneOf(value))
		}
	}
	c.Assert(properties.Values, qt.HasLen, 5)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	convertProperties(c, client, properties)
	t.Run("host export", func(t *testing.T) { assertHostSchemaExport(t, client, binary, digest, []string{directory}, all) })
}
