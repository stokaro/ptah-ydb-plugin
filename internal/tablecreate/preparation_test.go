package tablecreate_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
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
	c := testcheck.New(t)
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
	c := testcheck.New(t)
	request := preparationRequest(c)
	invalid := proto.CloneOf(request.Tables[0])
	invalid.Subject.Name = part("invalid")
	invalid.Desired.Name = "invalid"
	invalid.Desired.Features.Facets.Records = nil
	invalid.Desired.Columns[0].DefaultExpression = "CurrentUtcTimestamp()"
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

// indexKindRequest is preparationRequest with two indexes declared on value,
// a synchronous one in the built-in engine's spelling and an asynchronous
// one, for a host that lists schema.index-methods.
func indexKindRequest(c *qt.C) *protocol.PrepareTablesRequest {
	c.Helper()
	request := preparationRequest(c)
	request.TargetFeatures = []string{provider.IndexMethodsFeature}
	request.Tables[0].Desired.Indexes = []*protocol.DeclaredIndex{
		{Name: "by_value", Fields: []string{"value"}, Type: "GLOBAL SYNC"},
		{Name: "by_value_async", Fields: []string{"value"}, Type: "global_async"},
	}
	return request
}

// A host that lists schema.index-methods gets the kind of each declared
// index in the spelling inspection reports a held index's method with: none
// for a synchronous index and async for an asynchronous one, whatever the
// source wrote. A host that does not list it gets none.
func TestPrepareTablesStatesIndexKinds(t *testing.T) {
	for _, row := range []struct {
		name     string
		features []string
		want     []*protocol.PreparedIndex
	}{
		{"negotiated", []string{provider.IndexMethodsFeature}, []*protocol.PreparedIndex{{Name: "by_value"}, {Name: "by_value_async", Method: "async"}}},
		{"not negotiated", nil, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := indexKindRequest(c)
			request.TargetFeatures = row.features
			result, err := backend.New().PrepareTables(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Tables[0].Indexes, qt.HasLen, len(row.want))
			for i, want := range row.want {
				c.Assert(proto.Equal(result.Tables[0].Indexes[i], want), qt.IsTrue, qt.Commentf("%v", result.Tables[0].Indexes[i]))
			}
		})
	}
}

// A kind the built-in engine's spellings do not name refuses the
// preparation, as it refuses the creation.
func TestPrepareTablesRefusesAnUnknownIndexKind(t *testing.T) {
	c := testcheck.New(t)
	request := indexKindRequest(c)
	request.Tables[0].Desired.Indexes[1].Type = "hash"
	result, err := backend.New().PrepareTables(c.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(status.Convert(err).Message(), qt.Matches, `.*index method "hash" has no YDB counterpart.*`)
	c.Assert(result, qt.IsNil)
}
