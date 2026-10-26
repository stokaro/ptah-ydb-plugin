package tablecreate_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func columnSemanticsRequest(c *qt.C) *protocol.CompareColumnsRequest {
	c.Helper()
	plan := creationPlan(c)
	desired := plan.Tables[0].Desired
	desired.Features.Facets.Records = nil
	current := &protocol.ObservedTable{Name: desired.Name, Schema: desired.Schema, Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
	}, Columns: []*protocol.ObservedColumn{{Name: "held", DataType: "Uint64", IsNullable: "NO"}}}
	source := proto.CloneOf(plan.Tables[0].Subject)
	column := proto.CloneOf(source)
	column.Kind = "column"
	column.Parent = part(desired.Name)
	column.Name = part(desired.Columns[0].Name)
	held := proto.CloneOf(column)
	held.Name = part("held")
	return &protocol.CompareColumnsRequest{Engine: plan.Engine, Identifiers: plan.Identifiers, Capabilities: plan.Capabilities, TargetFeatures: []string{provider.ColumnsFeature}, Tables: []*protocol.ColumnComparisonTable{{Subject: source, CurrentSubject: proto.CloneOf(source), Desired: desired, Current: current, Pairs: []*protocol.ColumnComparisonPair{{Subject: column, CurrentSubject: held, DesiredColumn: desired.Columns[0].Name, CurrentColumn: "held", Aspects: []protocol.ColumnAspect{protocol.ColumnAspect_COLUMN_TYPE, protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnAspect_COLUMN_NULLABILITY, protocol.ColumnAspect_COLUMN_SEQUENCE}}}}}}
}

func TestColumnServicePreservesExactCaptures(t *testing.T) {
	c := qt.New(t)
	request := columnSemanticsRequest(c)
	request.Tables[0].Desired.Columns[0].Nullable = true
	before := proto.CloneOf(request)
	result, err := backend.New().CompareColumns(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedColumns(provider.NegotiatedDescription(backend.Description(), request.TargetFeatures), request, result), qt.IsNil)
	c.Assert(result.Tables[0].Columns[0].Aspects[0].GetDifference().Relation, qt.Equals, protocol.ColumnDifference_SAME)
	c.Assert(result.Tables[0].Columns[0].Aspects[2].GetNullability().Nullable, qt.DeepEquals, new(false))
	c.Assert(result.Tables[0].Columns[0].Aspects[3].GetSequence().Changes, qt.HasLen, 0)
	c.Assert(proto.Equal(request, before), qt.IsTrue)
}

func TestColumnServiceRefusesPartialOrUnboundInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.CompareColumnsRequest)
	}{
		{"unnegotiated", func(r *protocol.CompareColumnsRequest) { r.TargetFeatures = nil }},
		{"foreign current parent", func(r *protocol.CompareColumnsRequest) {
			r.Tables[0].Pairs[0].CurrentSubject.Parent = part("elsewhere")
		}},
		{"foreign normalized column", func(r *protocol.CompareColumnsRequest) { r.Tables[0].Pairs[0].Subject.Name.Normalized = "elsewhere" }},
		{"missing nullable observation", func(r *protocol.CompareColumnsRequest) { r.Tables[0].Current.Columns[0].IsNullable = "" }},
		{"invalid second table", func(r *protocol.CompareColumnsRequest) {
			r.Tables = append(r.Tables, proto.CloneOf(r.Tables[0]))
			r.Tables[1].Current = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := columnSemanticsRequest(c)
			test.mutate(request)
			result, err := backend.New().CompareColumns(c.Context(), request)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(result, qt.IsNil)
		})
	}
}

func TestColumnServiceReadbackAndCancellation(t *testing.T) {
	c := qt.New(t)
	compared := columnSemanticsRequest(c)
	pair := compared.Tables[0].Pairs[0]
	request := &protocol.ReadColumnDefaultsRequest{Engine: compared.Engine, Identifiers: compared.Identifiers, Capabilities: compared.Capabilities, TargetFeatures: compared.TargetFeatures,
		Columns: []*protocol.ObservedColumnDefaultInput{{Subject: pair.CurrentSubject, Column: &protocol.ObservedColumn{Name: "held", DataType: "Utf8", IsNullable: "YES", DefaultExpression: new("''u")}}}}
	result, err := backend.New().ReadColumnDefaults(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Columns[0].Kind, qt.Equals, protocol.ColumnDefaultReceipt_LITERAL)
	c.Assert(result.Columns[0].Value, qt.Equals, "")
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err = backend.New().ReadColumnDefaults(ctx, request)
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
	columns, err := backend.New().CompareColumns(ctx, compared)
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(columns, qt.IsNil)
}

func TestCreationColumnsMatchCompiledPrimaryKey(t *testing.T) {
	for _, nullable := range []bool{false, true} {
		t.Run(map[bool]string{false: "not null", true: "explicit nullable"}[nullable], func(t *testing.T) {
			c := qt.New(t)
			request := projectionRequest(c)
			request.TargetFeatures = []string{provider.ColumnsFeature, provider.CommonObjectsFeature}
			table := request.Tables[0].Declaration
			table.Columns[0].Nullable = nullable
			table.Columns[0].Comment = "retained"
			before := proto.CloneOf(request)
			projected, err := backend.New().ProjectTableCreations(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(projected.Tables[0].Columns, qt.HasLen, len(table.Columns))
			c.Assert(projected.Tables[0].Columns[0].IsNullable, qt.Equals, "NO")
			c.Assert(projected.Tables[0].Columns[0].PrimaryKey, qt.IsTrue)
			c.Assert(projected.Tables[0].Columns[0].Comment, qt.Equals, "retained")
			clean := proto.CloneOf(table)
			clean.Columns[0].Comment = ""
			compiled, err := tablecreate.Compile(owner(), clean, nil)
			c.Assert(err, qt.IsNil)
			c.Assert(compiled.Columns[0].GetNotNull(), qt.IsTrue)
			c.Assert(projected.Tables[0].Columns[2].IsNullable, qt.Equals, "YES")
			c.Assert(proto.Equal(request, before), qt.IsTrue)
		})
	}
}

func TestColumnParentRefusesUnplannedDefaultChange(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "changed"}[changed], func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, alterCase{declared: []string{"value"}, held: []string{"value"}, heldTable: plainTable})
			request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
			table := request.Tables[0]
			table.Current.Columns[0].DataType = "Uint64"
			table.Current.Columns[0].IsNullable = "NO"
			table.Current.Columns[1].DataType = "Utf8"
			table.Current.Columns[1].IsNullable = "YES"
			table.Current.Columns[1].DefaultExpression = new("'a'u")
			table.Desired.Columns[1].DefaultValue = "a"
			table.Desired.Columns[1].DefaultSet = true
			if changed {
				table.Desired.Columns[1].DefaultValue = "b"
			}
			plan, err := tablecreate.Plan(c.Context(), request)
			c.Assert(err, qt.IsNil)
			if changed {
				c.Assert(plan.Diagnostics, qt.HasLen, 1)
				c.Assert(plan.Diagnostics[0].Message, qt.Contains, "default change")
				c.Assert(plan.Parents, qt.HasLen, 0)
			} else {
				c.Assert(plan.Diagnostics, qt.HasLen, 0)
				c.Assert(plan.Parents, qt.HasLen, len(request.ParentKinds))
			}
		})
	}
}
