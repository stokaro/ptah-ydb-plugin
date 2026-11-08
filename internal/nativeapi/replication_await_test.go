package nativeapi_test

import (
	"context"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_Replication"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replicaScheme is a server on which the replication app/mirror stands once
// created is set, and its replica table app/rep is described after absent
// answers to it, or never for a negative absent.
type replicaScheme struct {
	Ydb_Scheme_V1.SchemeServiceClient
	created  *bool
	absent   int
	describe *int
}

func (s replicaScheme) ListDirectory(context.Context, *Ydb_Scheme.ListDirectoryRequest, ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	listing := &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}}
	if *s.created {
		listing.Children = []*Ydb_Scheme.Entry{{Name: "mirror", Type: Ydb_Scheme.Entry_REPLICATION}}
	}
	result, err := anypb.New(listing)
	return &Ydb_Scheme.ListDirectoryResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: result}}, err
}

func (s replicaScheme) DescribePath(context.Context, *Ydb_Scheme.DescribePathRequest, ...grpc.CallOption) (*Ydb_Scheme.DescribePathResponse, error) {
	*s.describe++
	if s.absent < 0 || *s.describe <= s.absent {
		return &Ydb_Scheme.DescribePathResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SCHEME_ERROR}}, nil
	}
	result, err := anypb.New(&Ydb_Scheme.DescribePathResult{Self: &Ydb_Scheme.Entry{Name: "rep", Type: Ydb_Scheme.Entry_TABLE}})
	return &Ydb_Scheme.DescribePathResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: result}}, err
}

// createdMirror is the description of app/mirror once YDB created it.
func createdMirror() replicationService {
	return replicationService{replication: &Ydb_Replication.DescribeReplicationResult{Self: &Ydb_Scheme.Entry{Name: "mirror", Type: Ydb_Scheme.Entry_REPLICATION},
		ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "localhost:2136", Database: "/local"},
		Items:            []*Ydb_Replication.DescribeReplicationResult_Item{{SourcePath: "/local/app/src", DestinationPath: "/local/app/rep"}},
		State:            &Ydb_Replication.DescribeReplicationResult_Running{Running: &Ydb_Replication.DescribeReplicationResult_RunningState{}}}}
}

// mirrorCreation is the operation that creates app/mirror the way operation
// names.
func mirrorCreation(c *qt.C, operation string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Schema: "app", Name: "mirror"}
	declared, err := schema.CaptureReplicationIntent(owner, replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local"},
		Items: []replication.Item{{Source: "app/src", Target: "app/rep"}}})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureReplicationChange("change", owner, nil, declared)
	c.Assert(err, qt.IsNil)
	value, err := schema.ReplicationOperationAs("operation", change.Value, operation)
	c.Assert(err, qt.IsNil)
	return value.Value
}

// replicaRun is what applying one creation did: whether it dispatched, the
// statements it ran and how often it described the replica table.
type replicaRun struct {
	dispatched bool
	statements int
	describes  int
}

// applyMirror applies the creation of app/mirror the way operation names on a
// server whose replica table appears after absent answers.
func applyMirror(c *qt.C, operation string, absent int) (replicaRun, error) {
	c.Helper()
	var run replicaRun
	created := false
	replications := &nativeapi.Replications{Scheme: replicaScheme{created: &created, absent: absent, describe: &run.describes}, Service: createdMirror(),
		Execute: func(context.Context, string) error {
			run.statements++
			created = true
			return nil
		}, ReplicaWait: 50 * time.Millisecond, ReplicaPoll: time.Millisecond}
	dispatched, err := replications.ApplyReplication(c.Context(), "/local", mirrorCreation(c, operation), func(context.Context) error { return nil })
	run.dispatched = dispatched
	return run, err
}

// A creation that awaits its replica tables runs its statement once and then
// describes the replica table until YDB reports it; a creation that goes on
// at once describes nothing. G231: a view over the replica in the same plan
// failed while the table was not there yet.
func TestApplyReplicationAwaitsItsReplicas_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		want      replicaRun
	}{
		{name: "awaits the replica table", operation: schema.ReplicationAwaitReplicas, want: replicaRun{dispatched: true, statements: 1, describes: 3}},
		{name: "goes on at once", operation: schema.ReplicationRun, want: replicaRun{dispatched: true, statements: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			run, err := applyMirror(c, test.operation, 2)

			c.Assert(err, qt.IsNil)
			c.Assert(run, qt.Equals, test.want)
		})
	}
}

// A replica table YDB has not created when the wait runs out is a
// ReplicasNotCreated error after the statement was dispatched, which names
// the replication and the table and says the plan stops.
func TestApplyReplicationAwaitsItsReplicas_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	run, err := applyMirror(c, schema.ReplicationAwaitReplicas, -1)

	c.Assert(err, qt.ErrorAs, new(*nativeapi.ReplicasNotCreated))
	c.Assert(err, qt.ErrorMatches, `async replication app/mirror was created, but YDB had not created its replica table app/rep after 50ms; `+
		`the plan reads that table next, so it stops here; run the plan again once the table exists`)
	c.Assert(run.dispatched, qt.IsTrue)
	c.Assert(run.statements, qt.Equals, 1)
}
