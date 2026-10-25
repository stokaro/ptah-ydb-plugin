//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

func TestCoordinationInspectionComparisonSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	native := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	absolute := path.Join(database, directory, "node")
	created, err := native.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: absolute, Config: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT}})
	c.Assert(err, qt.IsNil)
	c.Assert(created.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		dropped, err := native.DropNode(cleanup, &Ydb_Coordination.DropNodeRequest{Path: absolute})
		c.Check(err, qt.IsNil)
		c.Check(dropped.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.NativeObjects, qt.HasLen, 1)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	// Comparison remains available after the live session is closed.
	request, err := testcomparison.Request(&schema.CoordinationIntent{ReadConsistencyMode: new("strict")}, nil)
	c.Assert(err, qt.IsNil)
	desired := request.Desired.Records[0]
	desired.Subject.Schema = testcomparison.Part(directory)
	desired.Value.Value.Owner.Schema = directory
	desired.Value.NormalizedOwner.Schema = directory
	normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "observed", Representation: protocol.ValueInput_OBSERVED, Value: snapshot.NativeObjects[0]}}})
	c.Assert(err, qt.IsNil)
	request.Current.Records = []*protocol.ComparisonRecord{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(desired.Subject), Value: normalized.Values[0]}}
	// The single-directory inspection establishes this node, not all namespaces.
	request.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	request.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(desired.Subject), Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
	equal, err := client.CompareModels(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(equal.Changes, qt.HasLen, 0)
	// Every decision is made for a subject, so the namespace the read left
	// out raises none.
	c.Assert(equal.Undecided, qt.HasLen, 0)
	value, err := schema.CaptureCoordinationIntent(desired.Value.Value.Owner, schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	normalized, err = client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "desired", Representation: protocol.ValueInput_DESIRED, Value: value}}})
	c.Assert(err, qt.IsNil)
	desired.Value = normalized.Values[0]
	changed, err := client.CompareModels(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(changed.Changes, qt.HasLen, 1)
	c.Assert(changed.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(string(changed.Changes[0].Value.Value.Payload), qt.Contains, `"after":{}`)
	c.Assert(string(changed.Changes[0].Value.Value.Payload), qt.Contains, "CONSISTENCY_MODE_STRICT")
}
