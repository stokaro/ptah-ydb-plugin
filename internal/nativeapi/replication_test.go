package nativeapi_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_Replication"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replicationService answers every describe with one replication or one
// transfer, or with err.
type replicationService struct {
	replication *Ydb_Replication.DescribeReplicationResult
	transfer    *Ydb_Replication.DescribeTransferResult
	err         error
}

func (s replicationService) DescribeReplication(context.Context, *Ydb_Replication.DescribeReplicationRequest, ...grpc.CallOption) (*Ydb_Replication.DescribeReplicationResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	result, err := anypb.New(s.replication)
	return &Ydb_Replication.DescribeReplicationResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: result}}, err
}

func (s replicationService) DescribeTransfer(context.Context, *Ydb_Replication.DescribeTransferRequest, ...grpc.CallOption) (*Ydb_Replication.DescribeTransferResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	result, err := anypb.New(s.transfer)
	return &Ydb_Replication.DescribeTransferResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: result}}, err
}

func mirrorOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Database: "/local", Schema: "app", Name: "mirror"}
}

// TestCaptureReplication_HappyPath captures a described replication as its
// observed model, read relative to the root.
func TestCaptureReplication_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	service := replicationService{replication: &Ydb_Replication.DescribeReplicationResult{Self: &Ydb_Scheme.Entry{Name: "mirror", Type: Ydb_Scheme.Entry_REPLICATION},
		ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "localhost:2136", Database: "/local"},
		Items:            []*Ydb_Replication.DescribeReplicationResult_Item{{SourcePath: "/local/app/src", DestinationPath: "/local/app/rep"}},
		State:            &Ydb_Replication.DescribeReplicationResult_Running{Running: &Ydb_Replication.DescribeReplicationResult_RunningState{}}}}
	value, err := (&nativeapi.Replications{Service: service}).Capture(t.Context(), "/local", mirrorOwner())
	c.Assert(err, qt.IsNil)
	spec, state, err := schema.ObservedReplication(value)
	c.Assert(err, qt.IsNil)
	c.Assert(state, qt.Equals, replication.StateRunning)
	c.Assert(spec, qt.DeepEquals, replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local"},
		Items: []replication.Item{{Source: "app/src", Target: "app/rep"}}})
}

// TestCaptureReplication_FailurePath names why a replication is not read: a
// cluster that does not serve the replication API, and a description the
// provider cannot model, each with its own sentinel, which inspection turns
// into an uninspected subject.
func TestCaptureReplication_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		service replicationService
		want    error
		wantErr string
	}{
		{name: "no replication API", service: replicationService{err: status.Error(codes.Unimplemented, "unknown service")},
			want: nativeapi.ErrReplicationServiceUnavailable, wantErr: `describe YDB async replication: the YDB cluster does not serve the replication API .*`},
		{name: "a global level that commits at no interval", service: replicationService{replication: &Ydb_Replication.DescribeReplicationResult{
			Self: &Ydb_Scheme.Entry{Name: "mirror", Type: Ydb_Scheme.Entry_REPLICATION}, ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "localhost:2136", Database: "/local"},
			ConsistencyLevel: &Ydb_Replication.DescribeReplicationResult_GlobalConsistency{GlobalConsistency: &Ydb_Replication.ConsistencyLevelGlobal{CommitInterval: nil}},
			Items:            []*Ydb_Replication.DescribeReplicationResult_Item{{SourcePath: "/local/app/src", DestinationPath: "/local/app/rep"}}}},
			want: nativeapi.ErrUnreadable, wantErr: `the provider cannot read the description: .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := (&nativeapi.Replications{Service: test.service}).Capture(t.Context(), "/local", mirrorOwner())
			c.Assert(err, qt.ErrorIs, test.want)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}
