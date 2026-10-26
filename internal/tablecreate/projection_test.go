package tablecreate_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func projectionRequest(c *qt.C) *protocol.ProjectTableCreationsRequest {
	c.Helper()
	plan := creationPlan(c)
	return &protocol.ProjectTableCreationsRequest{Engine: plan.Engine, Identifiers: plan.Identifiers, Capabilities: plan.Capabilities,
		Tables: []*protocol.TableCreationInput{{Subject: plan.Tables[0].Subject, Declaration: plan.Tables[0].Desired,
			FacetOwners: []*protocol.ComparisonIdentity{proto.CloneOf(plan.Tables[0].Subject)}}}}
}

func TestCreationProjectionPreservesIntentAndUnknownDefaults(t *testing.T) {
	for _, test := range []struct {
		name   string
		native bool
	}{
		{"common key creates native intent", false}, {"native key and settings", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := projectionRequest(c)
			table := request.Tables[0].Declaration
			if test.native {
				table.PrimaryKey = nil
				for _, column := range table.Columns {
					column.Primary = false
				}
			} else {
				table.Features.Facets.Records = nil
			}
			original := proto.CloneOf(request)
			result, err := backend.New().ProjectTableCreations(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Complete, qt.IsTrue)
			c.Assert(result.Tables, qt.HasLen, 1)
			projected := result.Tables[0]
			c.Assert(projected.ColumnPrimaryKeysPrepared, qt.IsTrue)
			c.Assert(projected.ColumnPrimaryKeys, qt.DeepEquals, []string{"second", "first"})
			c.Assert(projected.ObservedFacets, qt.HasLen, 0)
			c.Assert(projected.DesiredFacets, qt.HasLen, 1)
			want := facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"]}`).Value.Value
			if test.native {
				want = table.Features.Facets.Records[0].Value.Value
			}
			c.Assert(proto.Equal(projected.DesiredFacets[0].Value.Value, want), qt.IsTrue)
			c.Assert(proto.Equal(original, request), qt.IsTrue)
			converted, err := backend.New().ConvertValues(c.Context(), &protocol.ConvertValuesRequest{
				Engine: request.Engine, From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED,
				Values: []*protocol.NormalizedValue{projected.DesiredFacets[0].Value},
			})
			c.Assert(err, qt.IsNil)
			c.Assert(converted.Values, qt.HasLen, 1)
			projected.ColumnPrimaryKeys[0] = "mutated"
			projected.Subject.Name.Source = "mutated"
			projected.DesiredFacets[0].Value.Value.Payload[0] = '!'
			c.Assert(proto.Equal(original, request), qt.IsTrue)
		})
	}
}

func TestCreationProjectionRetainsExclusion(t *testing.T) {
	c := qt.New(t)
	request := projectionRequest(c)
	table := request.Tables[0].Declaration
	excluded := table.Features.Facets.Records[0]
	excluded.Value = nil
	excluded.Targets = []string{"other"}
	table.Features.Facets.Records = []*protocol.ComparisonRecord{excluded}
	result, err := backend.New().ProjectTableCreations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Tables[0].ColumnPrimaryKeys, qt.DeepEquals, []string{"second", "first"})
	c.Assert(result.Tables[0].DesiredFacets, qt.HasLen, 0)
	c.Assert(result.Tables[0].ObservedFacets, qt.HasLen, 0)
}

func TestCreationProjectionDiscardsValidPrefix(t *testing.T) {
	c := qt.New(t)
	request := projectionRequest(c)
	invalid := proto.CloneOf(request.Tables[0])
	invalid.Subject.Name = part("invalid")
	invalid.FacetOwners = []*protocol.ComparisonIdentity{proto.CloneOf(invalid.Subject)}
	invalid.Declaration.Name = "invalid"
	invalid.Declaration.Features.Facets.Records = nil
	invalid.Declaration.Columns[0].DefaultExpression = "CurrentUtcTimestamp()"
	request.Tables = append(request.Tables, invalid)
	result, err := backend.New().ProjectTableCreations(c.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(result, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err = backend.New().ProjectTableCreations(ctx, projectionRequest(c))
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
}

func TestCreationProjectionConvertsBeforeDatabaseBinding(t *testing.T) {
	c := qt.New(t)
	request := projectionRequest(c)
	input := request.Tables[0]
	input.Subject.Database, input.Subject.Schema = nil, nil
	input.FacetOwners = []*protocol.ComparisonIdentity{proto.CloneOf(input.Subject)}
	input.Declaration.Schema = ""
	input.Declaration.Features.Facets.Records = nil
	p := backend.New()
	result, err := p.ProjectTableCreations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := result.Tables[0].DesiredFacets[0].Value
	observed, err := p.ConvertValues(c.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: []*protocol.NormalizedValue{value}})
	c.Assert(err, qt.IsNil)
	c.Assert(observed.Values[0].Value.Owner.Database, qt.Equals, "")
	c.Assert(observed.Values[0].Value.Owner.Schema, qt.Equals, "")
	restored, err := p.ConvertValues(c.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: observed.Values})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(restored.Values[0], value), qt.IsTrue)
}
