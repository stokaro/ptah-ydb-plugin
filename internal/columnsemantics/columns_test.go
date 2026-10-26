package columnsemantics_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func TestCapturedColumnAspects(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired *protocol.DeclaredColumn
		current *protocol.ObservedColumn
		aspect  protocol.ColumnAspect
		want    *protocol.ColumnAspectReceipt
	}{
		{"integer alias", &protocol.DeclaredColumn{Type: "BIGINT"}, &protocol.ObservedColumn{DataType: "Int64"}, protocol.ColumnAspect_COLUMN_TYPE, diff(protocol.ColumnAspect_COLUMN_TYPE, protocol.ColumnDifference_SAME, "Int64", "Int64")},
		{"wide timestamp accepts a narrow catalog", &protocol.DeclaredColumn{Type: "TIMESTAMP"}, &protocol.ObservedColumn{DataType: "Timestamp"}, protocol.ColumnAspect_COLUMN_TYPE, diff(protocol.ColumnAspect_COLUMN_TYPE, protocol.ColumnDifference_SAME, "Timestamp", "Timestamp")},
		{"integer width change", &protocol.DeclaredColumn{Type: "Int64"}, &protocol.ObservedColumn{DataType: "Int32"}, protocol.ColumnAspect_COLUMN_TYPE, diff(protocol.ColumnAspect_COLUMN_TYPE, protocol.ColumnDifference_CHANGED, "Int32", "Int64")},
		{"incrementing alias", &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true}, &protocol.ObservedColumn{DataType: "Int64", AutoIncrement: true}, protocol.ColumnAspect_COLUMN_TYPE, diff(protocol.ColumnAspect_COLUMN_TYPE, protocol.ColumnDifference_SAME, "BigSerial", "BigSerial")},
		{"typed numeric default", &protocol.DeclaredColumn{Type: "Int8", DefaultSet: true, DefaultValue: "+05"}, &protocol.ObservedColumn{DataType: "Int8", DefaultExpression: new("5t")}, protocol.ColumnAspect_COLUMN_DEFAULT, diff(protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnDifference_SAME, "5t", "5t")},
		{"empty literal", &protocol.DeclaredColumn{Type: "Utf8", DefaultSet: true}, &protocol.ObservedColumn{DataType: "Utf8", DefaultExpression: new("''u")}, protocol.ColumnAspect_COLUMN_DEFAULT, diff(protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnDifference_SAME, "''u", "''u")},
		{"quoted string and cast", &protocol.DeclaredColumn{Type: "Utf8", DefaultSet: true, DefaultValue: "' a''b '::text"}, &protocol.ObservedColumn{DataType: "Utf8", DefaultExpression: new("' a\\'b 'u")}, protocol.ColumnAspect_COLUMN_DEFAULT, diff(protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnDifference_SAME, "' a\\'b 'u", "' a\\'b 'u")},
		{"NULL default", &protocol.DeclaredColumn{Type: "Int64", DefaultSet: true, DefaultValue: "NULL"}, &protocol.ObservedColumn{DataType: "Int64"}, protocol.ColumnAspect_COLUMN_DEFAULT, diff(protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnDifference_SAME, "", "")},
		{"expression", &protocol.DeclaredColumn{Type: "Timestamp", DefaultExpression: "CurrentUtcTimestamp()"}, &protocol.ObservedColumn{DataType: "Timestamp", DefaultExpression: new("CurrentUtcTimestamp()")}, protocol.ColumnAspect_COLUMN_DEFAULT, diff(protocol.ColumnAspect_COLUMN_DEFAULT, protocol.ColumnDifference_SAME, "CurrentUtcTimestamp()", "CurrentUtcTimestamp()")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			test.desired.Name = "desired"
			test.current.Name = "held"
			request := requestFor(test.desired, test.current, test.aspect)
			before := proto.CloneOf(request)
			result, err := columnsemantics.Compare(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Complete, qt.IsTrue)
			c.Assert(result.Tables, qt.HasLen, 1)
			c.Assert(result.Tables[0].Columns, qt.HasLen, 1)
			got := result.Tables[0].Columns[0]
			c.Assert(proto.Equal(got.Subject, request.Tables[0].Pairs[0].Subject), qt.IsTrue)
			c.Assert(proto.Equal(got.CurrentSubject, request.Tables[0].Pairs[0].CurrentSubject), qt.IsTrue)
			c.Assert(got.Aspects, qt.HasLen, 1)
			c.Assert(proto.Equal(got.Aspects[0], test.want), qt.IsTrue, qt.Commentf("got %v want %v", got.Aspects[0], test.want))
			c.Assert(proto.Equal(request, before), qt.IsTrue)
		})
	}
}

func TestPrimaryAndSerialNullabilityDoesNotRewriteSource(t *testing.T) {
	for _, test := range []struct {
		name     string
		column   *protocol.DeclaredColumn
		keys     []string
		nullable bool
	}{
		{"ordinary nullable", &protocol.DeclaredColumn{Name: "id", Type: "Int64", Nullable: true}, nil, true},
		{"ordinary not null", &protocol.DeclaredColumn{Name: "id", Type: "Int64"}, nil, false},
		{"explicit nullable primary", &protocol.DeclaredColumn{Name: "id", Type: "Int64", Nullable: true, Primary: true}, nil, false},
		{"parent primary", &protocol.DeclaredColumn{Name: "id", Type: "Int64", Nullable: true}, []string{"id"}, false},
		{"serial nullable", &protocol.DeclaredColumn{Name: "id", Type: "BigSerial", Nullable: true}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := requestFor(test.column, &protocol.ObservedColumn{Name: "id", DataType: "Int64"}, protocol.ColumnAspect_COLUMN_NULLABILITY)
			request.Tables[0].Desired.PrimaryKey = test.keys
			before := proto.CloneOf(request)
			result, err := columnsemantics.Compare(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Tables[0].Columns[0].Aspects[0].GetNullability().GetNullable(), qt.Equals, test.nullable)
			c.Assert(proto.Equal(request, before), qt.IsTrue)
		})
	}
}

func TestSequenceComparisonIgnoresRuntimeRestart(t *testing.T) {
	c := qt.New(t)
	request := requestFor(&protocol.DeclaredColumn{Name: "id", Type: "BIGINT", AutoIncrement: true, IdentityStart: "010", IdentityIncrement: "+2"}, &protocol.ObservedColumn{Name: "id", DataType: "Int64", AutoIncrement: true, IdentityStart: "10", IdentityIncrement: "1", SequenceRestart: "999"}, protocol.ColumnAspect_COLUMN_SEQUENCE)
	result, err := columnsemantics.Compare(c.Context(), request)
	c.Assert(err, qt.IsNil)
	changes := result.Tables[0].Columns[0].Aspects[0].GetSequence().Changes
	c.Assert(changes, qt.HasLen, 1)
	c.Assert(changes[0].Attribute, qt.Equals, protocol.ColumnSequenceDifference_IDENTITY_INCREMENT)
	c.Assert(changes[0].Current, qt.Equals, "1")
	c.Assert(changes[0].Desired, qt.Equals, "2")
}

func TestDefaultReadbackPreservesLiteralPresence(t *testing.T) {
	for _, test := range []struct {
		name  string
		text  *string
		kind  protocol.ColumnDefaultReceipt_Kind
		value string
	}{
		{"absent", nil, protocol.ColumnDefaultReceipt_ABSENT, ""},
		{"empty string", new("''u"), protocol.ColumnDefaultReceipt_LITERAL, ""},
		{"integer", new("5t"), protocol.ColumnDefaultReceipt_LITERAL, "5"},
		{"expression", new("CurrentUtcTimestamp()"), protocol.ColumnDefaultReceipt_EXPRESSION, "CurrentUtcTimestamp()"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := columnsemantics.ReadDefaults(c.Context(), &protocol.ReadColumnDefaultsRequest{Columns: []*protocol.ObservedColumnDefaultInput{{Column: &protocol.ObservedColumn{DefaultExpression: test.text}}}})
			c.Assert(err, qt.IsNil)
			c.Assert(result.Columns, qt.HasLen, 1)
			c.Assert(result.Columns[0].Kind, qt.Equals, test.kind)
			c.Assert(result.Columns[0].Value, qt.Equals, test.value)
		})
	}
}

func TestColumnSemanticsCancellationHasNoPrefix(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	request := requestFor(&protocol.DeclaredColumn{Name: "id", Type: "Int64"}, &protocol.ObservedColumn{Name: "id", DataType: "Int64"}, protocol.ColumnAspect_COLUMN_TYPE)
	compared, err := columnsemantics.Compare(ctx, request)
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(compared, qt.IsNil)
	defaults, err := columnsemantics.ReadDefaults(ctx, &protocol.ReadColumnDefaultsRequest{Columns: []*protocol.ObservedColumnDefaultInput{{Column: request.Tables[0].Current.Columns[0]}}})
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(defaults, qt.IsNil)
}

func requestFor(desired *protocol.DeclaredColumn, current *protocol.ObservedColumn, aspect protocol.ColumnAspect) *protocol.CompareColumnsRequest {
	return &protocol.CompareColumnsRequest{Tables: []*protocol.ColumnComparisonTable{{Desired: &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{desired}}, Current: &protocol.ObservedTable{Columns: []*protocol.ObservedColumn{current}}, Pairs: []*protocol.ColumnComparisonPair{{Subject: &protocol.ComparisonIdentity{Name: &protocol.ComparisonPart{Source: desired.Name, Normalized: desired.Name}}, CurrentSubject: &protocol.ComparisonIdentity{Name: &protocol.ComparisonPart{Source: current.Name, Normalized: current.Name}}, DesiredColumn: desired.Name, CurrentColumn: current.Name, Aspects: []protocol.ColumnAspect{aspect}}}}}}
}
func diff(aspect protocol.ColumnAspect, relation protocol.ColumnDifference_Relation, current, desired string) *protocol.ColumnAspectReceipt {
	return &protocol.ColumnAspectReceipt{Aspect: aspect, Receipt: &protocol.ColumnAspectReceipt_Difference{Difference: &protocol.ColumnDifference{Relation: relation, Current: current, Desired: desired}}}
}
