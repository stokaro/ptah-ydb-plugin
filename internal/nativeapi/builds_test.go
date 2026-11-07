package nativeapi_test

import (
	"context"
	"errors"
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
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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
			c := testcheck.New(t)
			service := &operations{builds: test.builds, final: test.final}

			outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(context.Background(), "t", quick)

			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, test.outcome)
			c.Assert(service.canceled, qt.DeepEquals, test.canceled)
		})
	}
}

// pagedOperations is an operation service that lists its builds on pages, as
// the server does: it numbers its pages from 2, answers page "0" with the first
// page, and ends a listing with the token "0". GetOperation answers each build
// from a script, one state a call, and repeats the last state. A build in
// late is listed from the second listing on, as a build its statement had not
// started when the first listing ran.
type pagedOperations struct {
	Ydb_Operation_V1.OperationServiceClient
	pages    [][]*Ydb_Operations.Operation
	late     *Ydb_Operations.Operation
	listErr  error
	states   map[string][]*Ydb_Operations.Operation
	canceled []string
	listings int
}

func (p *pagedOperations) ListOperations(_ context.Context, request *Ydb_Operations.ListOperationsRequest, _ ...grpc.CallOption) (*Ydb_Operations.ListOperationsResponse, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	page := 0
	if request.GetPageToken() == "2" {
		page = 1
	} else {
		p.listings++
	}
	pages := p.pages
	if p.late != nil && p.listings > 1 {
		pages = [][]*Ydb_Operations.Operation{{p.late}}
	}
	response := &Ydb_Operations.ListOperationsResponse{Status: Ydb.StatusIds_SUCCESS, NextPageToken: "0"}
	if page < len(pages) {
		response.Operations = pages[page]
	}
	if page+1 < len(pages) {
		response.NextPageToken = "2"
	}
	return response, nil
}

func (p *pagedOperations) CancelOperation(_ context.Context, request *Ydb_Operations.CancelOperationRequest, _ ...grpc.CallOption) (*Ydb_Operations.CancelOperationResponse, error) {
	p.canceled = append(p.canceled, request.GetId())
	return &Ydb_Operations.CancelOperationResponse{Status: Ydb.StatusIds_SUCCESS}, nil
}

func (p *pagedOperations) GetOperation(_ context.Context, request *Ydb_Operations.GetOperationRequest, _ ...grpc.CallOption) (*Ydb_Operations.GetOperationResponse, error) {
	script := p.states[request.GetId()]
	if len(script) == 0 {
		return nil, errors.New("read a build the fixture does not hold")
	}
	state := script[0]
	if len(script) > 1 {
		p.states[request.GetId()] = script[1:]
	}
	return &Ydb_Operations.GetOperationResponse{Operation: state}, nil
}

// build is a build operation on the table at path, running or ended with
// status.
func build(c *qt.C, id, path string, ready bool, status Ydb.StatusIds_StatusCode) *Ydb_Operations.Operation {
	c.Helper()
	metadata, err := anypb.New(&Ydb_Table.IndexBuildMetadata{Description: &Ydb_Table.IndexBuildDescription{Path: path}})
	c.Assert(err, qt.IsNil)
	return &Ydb_Operations.Operation{Id: id, Ready: ready, Status: status, Metadata: metadata}
}

// Ported from the host's TestBuilds_CancelRunning_HappyPath: the build
// canceled is the one running on the table, found on a later page past an
// ended build and another table's, and matched by its absolute path too. The
// outcome is what the build reported after the cancellation; one that failed
// rather than being canceled has ended without completing as well.
func TestCancelRunningBuilds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		table string
		after Ydb.StatusIds_StatusCode
		want  nativeapi.BuildOutcome
	}{
		{name: "canceled", table: "dir/items", after: Ydb.StatusIds_CANCELLED, want: nativeapi.BuildStopped},
		{name: "failed", table: "dir/items", after: Ydb.StatusIds_GENERIC_ERROR, want: nativeapi.BuildStopped},
		{name: "completed first", table: "dir/items", after: Ydb.StatusIds_SUCCESS, want: nativeapi.BuildCompleted},
		{name: "an absolute path", table: "/local/dir/items", after: Ydb.StatusIds_CANCELLED, want: nativeapi.BuildStopped},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			service := &pagedOperations{
				pages: [][]*Ydb_Operations.Operation{
					{build(c, "done", "/local/dir/items", true, Ydb.StatusIds_SUCCESS), build(c, "other", "/local/dir/other", false, 0)},
					{build(c, "ours", "/local/dir/items", false, 0)},
				},
				states: map[string][]*Ydb_Operations.Operation{
					"ours": {build(c, "ours", "/local/dir/items", false, 0), build(c, "ours", "/local/dir/items", true, test.after)},
				},
			}

			outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(c.Context(), test.table, quick)

			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, test.want)
			c.Assert(service.canceled, qt.DeepEquals, []string{"ours"})
		})
	}
}

// Ported from the host's TestBuilds_CancelRunning_WaitsForTheBuildToAppear: a
// build that appears only after the first look is still found and canceled.
func TestCancelRunningBuilds_WaitsForTheBuildToAppear(t *testing.T) {
	c := testcheck.New(t)
	service := &pagedOperations{
		late:   build(c, "late", "/local/items", false, 0),
		states: map[string][]*Ydb_Operations.Operation{"late": {build(c, "late", "/local/items", true, Ydb.StatusIds_CANCELLED)}},
	}

	outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(c.Context(), "items", quick)

	c.Assert(err, qt.IsNil)
	c.Assert(outcome, qt.Equals, nativeapi.BuildStopped)
	c.Assert(service.canceled, qt.DeepEquals, []string{"late"})
	c.Assert(service.listings > 1, qt.IsTrue)
}

// Ported from the host's TestBuilds_CancelRunning_FailurePath: a canceled
// build that does not end within the wait is reported as unsettled, never as
// stopped.
func TestCancelRunningBuilds_ACanceledBuildThatDoesNotEnd(t *testing.T) {
	c := testcheck.New(t)
	service := &pagedOperations{
		pages:  [][]*Ydb_Operations.Operation{{build(c, "elsewhere", "/local/other", false, 0), build(c, "a", "/local/items", false, 0)}},
		states: map[string][]*Ydb_Operations.Operation{"a": {build(c, "a", "/local/items", false, 0)}},
	}

	outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(c.Context(), "items", quick)

	c.Assert(err, qt.IsNil)
	c.Assert(outcome, qt.Equals, nativeapi.BuildUnsettled)
	c.Assert(service.canceled, qt.DeepEquals, []string{"a"})
}

// Ported from the host's TestBuilds_CancelRunning_ListFails: an operation
// service that cannot be asked fails the call, which says nothing about the
// build, and nothing is canceled.
func TestCancelRunningBuilds_ListFails(t *testing.T) {
	c := testcheck.New(t)
	service := &pagedOperations{listErr: errors.New("transport: connection refused")}

	outcome, err := nativeapi.NewBuilds(service, "/local").CancelRunning(c.Context(), "items", quick)

	c.Assert(err, qt.ErrorMatches, `list the YDB builds: transport failed`)
	c.Assert(outcome, qt.Equals, nativeapi.BuildUnsettled)
	c.Assert(service.canceled, qt.HasLen, 0)
}
