package tablecreate_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func preparationRequest(c *qt.C) *protocol.PrepareTablesRequest {
	c.Helper()
	plan := creationPlan(c)
	return &protocol.PrepareTablesRequest{Engine: plan.Engine, Identifiers: plan.Identifiers, Capabilities: plan.Capabilities,
		Tables: []*protocol.TablePreparationInput{{Subject: plan.Tables[0].Subject, Desired: plan.Tables[0].Desired,
			CurrentKnowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_ABSENT}}}}
}

func TestPrepareNativeTablePreservesSource(t *testing.T) {
	c := qt.New(t)
	request := preparationRequest(c)
	request.Tables[0].Desired.PrimaryKey = nil
	for _, column := range request.Tables[0].Desired.Columns {
		column.Primary = false
	}
	request.Tables[0].CurrentKnowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Offline source only"}
	original := proto.CloneOf(request)
	result, err := backend.New().PrepareTables(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Tables, qt.HasLen, 1)
	c.Assert(result.Tables[0].ColumnPrimaryKeysPrepared, qt.IsTrue)
	c.Assert(result.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, []string{"second", "first"})
	c.Assert(result.Tables[0].ResolvedFacets, qt.HasLen, 0)
	c.Assert(proto.Equal(original, request), qt.IsTrue)
	result.Tables[0].Subject.Name.Source = "mutated"
	result.Tables[0].ColumnPrimaryKeys[0] = "mutated"
	c.Assert(proto.Equal(original, request), qt.IsTrue)
}

func TestPrepareNativeTableRefusesWholeBatch(t *testing.T) {
	c := qt.New(t)
	request := preparationRequest(c)
	invalid := proto.CloneOf(request.Tables[0])
	invalid.Subject.Name = part("invalid")
	invalid.Desired.Name = "invalid"
	invalid.Desired.Features.Facets.Records = nil
	invalid.Desired.Columns[0].DefaultSet = true
	invalid.Desired.Columns[0].DefaultValue = "2"
	request.Tables = append(request.Tables, invalid)
	result, err := backend.New().PrepareTables(c.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(result, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err = backend.New().PrepareTables(ctx, preparationRequest(c))
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
}
