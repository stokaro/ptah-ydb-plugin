//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	providerruntime "ptah.run/provider-sdk/runtime"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func convertProperties(c *qt.C, client *providerruntime.Client, request *protocol.NormalizeValuesRequest) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	observed, err := client.NormalizeValues(ctx, request)
	c.Assert(err, qt.IsNil)
	conversion := &protocol.ConvertValuesRequest{Engine: "ydb", TargetFeatures: request.TargetFeatures, From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: observed.Values}
	desired, err := client.ConvertValues(ctx, conversion)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateConvertedValues(client.Description(), conversion, desired), qt.IsNil)
	c.Assert(desired.Values, qt.HasLen, len(request.Values))
	for _, value := range desired.Values {
		definitions := map[string]string{schema.ColumnProperties: schema.ColumnDesiredModel().DefinitionSha256, schema.TableProperties: schema.TableDesiredModel().DefinitionSha256}
		c.Assert(value.Value.DefinitionSha256, qt.Equals, definitions[value.Value.Kind])
	}
	back, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", TargetFeatures: request.TargetFeatures, From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: desired.Values})
	c.Assert(err, qt.IsNil)
	// A declaration leaves out the storage default a read reports, so the read
	// does not come back byte for byte; the declaration written from it does.
	again, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", TargetFeatures: request.TargetFeatures, From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: back.Values})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(desired, again), qt.IsTrue)
}

func TestColumnConversionOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "App", Kind: "column", Path: []string{"Orders"}, Name: "Id"}
	observed, err := schema.CaptureColumn(owner, &native.ColumnMeta{Family: "cold", DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{Name: new("_serial"), Increment: proto.Int64(1)}}})
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	convertProperties(c, client, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "serial", Representation: protocol.ValueInput_OBSERVED, Value: observed}}})
}

func TestColumnConversionLiveSubprocess(t *testing.T) {
	c := testcheck.New(t)
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
	columns := &protocol.NormalizeValuesRequest{Engine: all.Engine, TargetFeatures: all.TargetFeatures}
	for _, value := range all.Values {
		if value.Value.Kind == schema.ColumnProperties {
			columns.Values = append(columns.Values, proto.CloneOf(value))
		}
	}
	c.Assert(columns.Values, qt.HasLen, 4)
	// Conversion must keep working after the only database session is closed.
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	convertProperties(c, client, columns)
}
