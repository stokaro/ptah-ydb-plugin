package nativeapi_test

import (
	"context"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
)

// operations is an operation service that lists the builds it holds, and
// ends a canceled one the way final says.
type operations struct {
	Ydb_Operation_V1.OperationServiceClient
	builds   map[string]string
	final    Ydb.StatusIds_StatusCode
	canceled []string
}

func (o *operations) ListOperations(_ context.Context, request *Ydb_Operations.ListOperationsRequest, _ ...grpc.CallOption) (*Ydb_Operations.ListOperationsResponse, error) {
	response := &Ydb_Operations.ListOperationsResponse{Status: Ydb.StatusIds_SUCCESS, NextPageToken: "0"}
	for id, table := range o.builds {
		metadata, err := anypb.New(&Ydb_Table.IndexBuildMetadata{Description: &Ydb_Table.IndexBuildDescription{Path: table}})
		if err != nil {
			return nil, err
		}
		response.Operations = append(response.Operations, &Ydb_Operations.Operation{Id: id, Metadata: metadata})
	}
	return response, nil
}

func (o *operations) CancelOperation(_ context.Context, request *Ydb_Operations.CancelOperationRequest, _ ...grpc.CallOption) (*Ydb_Operations.CancelOperationResponse, error) {
	o.canceled = append(o.canceled, request.GetId())
	return &Ydb_Operations.CancelOperationResponse{Status: Ydb.StatusIds_SUCCESS}, nil
}

func (o *operations) GetOperation(_ context.Context, request *Ydb_Operations.GetOperationRequest, _ ...grpc.CallOption) (*Ydb_Operations.GetOperationResponse, error) {
	return &Ydb_Operations.GetOperationResponse{Operation: &Ydb_Operations.Operation{Id: request.GetId(), Ready: true, Status: o.final}}, nil
}

var quick = nativeapi.BuildWait{Appear: 20 * time.Millisecond, Settle: 20 * time.Millisecond, Poll: time.Millisecond}

// A build is canceled only where exactly one runs on the table, and the
// outcome is what the operation service says the build ended as.
func TestCancelRunningBuilds(t *testing.T) {
	for _, test := range []struct {
		name     string
		builds   map[string]string
		final    Ydb.StatusIds_StatusCode
		outcome  nativeapi.BuildOutcome
		canceled []string
	}{
		{name: "none on the table", builds: map[string]string{"b1": "/local/other"}, outcome: nativeapi.BuildNotFound},
		{name: "one, canceled", builds: map[string]string{"b1": "/local/t", "b2": "/local/other"}, final: Ydb.StatusIds_CANCELLED,
			outcome: nativeapi.BuildStopped, canceled: []string{"b1"}},
		{name: "one, completed first", builds: map[string]string{"b1": "/local/t"}, final: Ydb.StatusIds_SUCCESS,
			outcome: nativeapi.BuildCompleted, canceled: []string{"b1"}},
		{name: "two on the table", builds: map[string]string{"b1": "/local/t", "b2": "/local/t"}, outcome: nativeapi.BuildUnsettled},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			service := &operations{builds: test.builds, final: test.final}

			outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(context.Background(), "t", quick)

			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, test.outcome)
			c.Assert(service.canceled, qt.DeepEquals, test.canceled)
		})
	}
}
