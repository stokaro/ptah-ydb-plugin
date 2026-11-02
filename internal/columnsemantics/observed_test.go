package columnsemantics_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

func TestObservedColumnPreservesCommonMetadata(t *testing.T) {
	for _, test := range []struct {
		name, spelling             string
		nullable, notNull, primary bool
		literal                    *protocol.DeclaredColumn
		want                       *protocol.ObservedColumn
	}{
		{name: "optional", spelling: "Utf8", nullable: true, want: &protocol.ObservedColumn{IsNullable: "YES"}},
		{name: "explicit not null", spelling: "Int64", nullable: true, notNull: true, primary: true, want: &protocol.ObservedColumn{IsNullable: "NO", PrimaryKey: true}},
		{name: "decimal", spelling: "Decimal(22,9)", want: &protocol.ObservedColumn{IsNullable: "NO", NumericPrecision: new(int64(22)), NumericScale: new(int64(9))}},
		{name: "empty literal", spelling: "Utf8", literal: &protocol.DeclaredColumn{DefaultSet: true}, want: &protocol.ObservedColumn{IsNullable: "NO", DefaultExpression: new("''u")}},
		{name: "integer literal", spelling: "Int64", literal: &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "5"}, want: &protocol.ObservedColumn{IsNullable: "NO", DefaultExpression: new("5l")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			kind, err := schema.StorageType(test.spelling, test.nullable)
			c.Assert(err, qt.IsNil)
			column := &native.ColumnMeta{Name: "value", Type: kind, NotNull: new(test.notNull)}
			if test.literal != nil {
				value, err := columnsemantics.NativeDefault(test.literal, test.spelling, nil)
				c.Assert(err, qt.IsNil)
				column.DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: value}
			}
			before := proto.CloneOf(column)
			got, err := columnsemantics.Observed(column, 3, test.primary)
			c.Assert(err, qt.IsNil)
			want := proto.CloneOf(test.want)
			want.Name, want.OrdinalPosition, want.DataType, want.ColumnType = "value", 3, test.spelling, test.spelling
			c.Assert(proto.Equal(got, want), qt.IsTrue, qt.Commentf("got %v; want %v", got, want))
			c.Assert(proto.Equal(column, before), qt.IsTrue)
		})
	}
}

func TestObservedSequenceRetainsConfigurationAndRestartEvidence(t *testing.T) {
	for _, test := range []struct {
		name                      string
		sequence                  *native.SequenceDescription
		start, increment, restart string
	}{
		{name: "implicit defaults", sequence: &native.SequenceDescription{}},
		{name: "explicit defaults", sequence: &native.SequenceDescription{StartValue: new(int64(1)), Increment: new(int64(1))}},
		{name: "settings and restart", sequence: &native.SequenceDescription{Name: new("_serial_column_id"), MinValue: new(int64(1)), MaxValue: new(int64(math.MaxInt64)), Cache: new(uint64(1)), StartValue: new(int64(7)), Increment: new(int64(3)), SetVal: &native.SequenceDescription_SetVal{NextValue: new(int64(11)), NextUsed: new(true)}}, start: "7", increment: "3", restart: "11"},
		{name: "present zero restart", sequence: &native.SequenceDescription{SetVal: &native.SequenceDescription_SetVal{NextValue: new(int64(0))}}, restart: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			kind, err := schema.StorageType("Int32", false)
			c.Assert(err, qt.IsNil)
			column := &native.ColumnMeta{Name: "id", Type: kind, DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: test.sequence}}
			got, err := columnsemantics.Observed(column, 1, true)
			c.Assert(err, qt.IsNil)
			want := &protocol.ObservedColumn{Name: "id", DataType: "Int32", ColumnType: "Int32", IsNullable: "NO", OrdinalPosition: 1, PrimaryKey: true, AutoIncrement: true, IdentityStart: test.start, IdentityIncrement: test.increment, SequenceRestart: test.restart}
			c.Assert(proto.Equal(got, want), qt.IsTrue, qt.Commentf("got %v; want %v", got, want))
		})
	}
}

func TestObservedColumnRefusesUnrepresentableFacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence *native.SequenceDescription
	}{
		{"foreign sequence", &native.SequenceDescription{Name: new("other")}},
		{"minimum", &native.SequenceDescription{MinValue: new(int64(0))}},
		{"maximum", &native.SequenceDescription{MaxValue: new(int64(20))}},
		{"cache", &native.SequenceDescription{Cache: new(uint64(2))}},
		{"cycle", &native.SequenceDescription{Cycle: new(true)}},
		{"sequence type", &native.SequenceDescription{DataType: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			kind, err := schema.StorageType("Int64", false)
			c.Assert(err, qt.IsNil)
			got, err := columnsemantics.Observed(&native.ColumnMeta{Name: "id", Type: kind, DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: test.sequence}}, 1, true)
			c.Assert(err, qt.IsNotNil)
			c.Assert(got, qt.IsNil)
		})
	}
}

func TestObservedColumnRejectsUnknownOrContradictoryMetadata(t *testing.T) {
	c := qt.New(t)
	kind, err := schema.StorageType("Int32", false)
	c.Assert(err, qt.IsNil)
	literal, err := columnsemantics.NativeDefault(&protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "wrong type"}, "Utf8", nil)
	c.Assert(err, qt.IsNil)
	unknown := &native.ColumnMeta{Name: "value", Type: kind}
	unknown.ProtoReflect().SetUnknown([]byte{0x80, 0x06, 0x01})
	for _, column := range []*native.ColumnMeta{nil, {Type: kind}, {Name: "value"}, unknown, {Name: "value", Type: kind, DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: literal}}} {
		got, err := columnsemantics.Observed(column, 1, false)
		c.Assert(err, qt.IsNotNil)
		c.Assert(got, qt.IsNil)
	}
}

func TestProjectedColumnUsesInspectionSemantics(t *testing.T) {
	c := qt.New(t)
	kind, err := schema.StorageType("Decimal(22,9)", true)
	c.Assert(err, qt.IsNil)
	projected, err := columnsemantics.Projected(&Ydb_Table.ColumnMeta{Name: "amount", Type: kind}, 2, false)
	c.Assert(err, qt.IsNil)
	observed, err := columnsemantics.Observed(&native.ColumnMeta{Name: "amount", Type: kind}, 2, false)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(projected, observed), qt.IsTrue)
	c.Assert(projected.GetNumericPrecision(), qt.Equals, int64(22))
	c.Assert(projected.GetNumericScale(), qt.Equals, int64(9))
}
