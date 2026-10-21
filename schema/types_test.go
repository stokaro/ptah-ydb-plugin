package schema_test

import (
	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
)

func primitive(id Ydb.Type_PrimitiveTypeId) *Ydb.Type {
	return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}}
}
func optional(value *Ydb.Type) *Ydb.Type {
	return &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: value}}}
}

func TestNativeStorageTypes(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    *Ydb.Type
		spelling string
		nullable bool
	}{
		{"bytes", primitive(Ydb.Type_STRING), "String", false},
		{"nullable UTF8", optional(primitive(Ydb.Type_UTF8)), "Utf8", true},
		{"narrow integer", primitive(Ydb.Type_INT8), "Int8", false},
		{"wide timestamp", primitive(Ydb.Type_TIMESTAMP64), "Timestamp64", false},
		{"Decimal", &Ydb.Type{Type: &Ydb.Type_DecimalType{DecimalType: &Ydb.DecimalType{Precision: 22, Scale: 9}}}, "Decimal(22,9)", false},
		{"nullable Decimal", optional(&Ydb.Type{Type: &Ydb.Type_DecimalType{DecimalType: &Ydb.DecimalType{Precision: 35, Scale: 0}}}), "Decimal(35,0)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			name, nullable, err := schema.ColumnType(test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(name, qt.Equals, test.spelling)
			c.Assert(nullable, qt.Equals, test.nullable)
		})
	}
}

func TestMalformedNativeTypes(t *testing.T) {
	for _, test := range []struct {
		name  string
		value *Ydb.Type
	}{
		{"nil", nil}, {"empty", &Ydb.Type{}}, {"unknown enum", primitive(Ydb.Type_PrimitiveTypeId(123456))},
		{"nested optional", optional(optional(primitive(Ydb.Type_UTF8)))},
		{"missing optional item", optional(nil)},
		{"wide Decimal", &Ydb.Type{Type: &Ydb.Type_DecimalType{DecimalType: &Ydb.DecimalType{Precision: 36, Scale: 9}}}},
		{"bad Decimal scale", &Ydb.Type{Type: &Ydb.Type_DecimalType{DecimalType: &Ydb.DecimalType{Precision: 2, Scale: 3}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, _, err := schema.ColumnType(test.value)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

func TestUnknownNestedTypeFields(t *testing.T) {
	c := qt.New(t)
	value := primitive(Ydb.Type_UTF8)
	value.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 400, protowire.VarintType), 1))
	_, _, err := schema.ColumnType(optional(value))
	c.Assert(err, qt.ErrorMatches, "unknown fields in Ydb.Type")
}
