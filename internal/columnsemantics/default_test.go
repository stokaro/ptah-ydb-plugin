package columnsemantics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
)

func TestNativeDefaultRoundTrip(t *testing.T) {
	for _, test := range []struct{ kind, value, literal string }{
		{"Int8", "+05", "5t"}, {"Int16", "-31", "-31s"}, {"Int32", "7", "7"}, {"Int64", "8", "8l"},
		{"Uint8", "5", "5ut"}, {"Uint16", "6", "6us"}, {"Uint32", "7", "7u"}, {"Uint64", "8", "8ul"},
		{"Bool", "yes", "true"}, {"Float", "1.5", "Float('1.5')"}, {"Double", "1.5", "Double('1.5')"},
		{"Utf8", "", "''u"}, {"String", "a'b", "'a\\'b'"}, {"Utf8", "' a''b '::text", "' a\\'b 'u"},
		{"Json", "{}", "Json('{}')"}, {"JsonDocument", "{}", "JsonDocument('{}')"}, {"DyNumber", "1.5", "DyNumber('1.5')"},
		{"Uuid", "00112233-4455-6677-8899-aabbccddeeff", "Uuid('00112233-4455-6677-8899-aabbccddeeff')"},
		{"Date", "2026-01-02", "Date('2026-01-02')"}, {"Date32", "1960-01-02", "Date32('1960-01-02')"},
		{"Datetime", "2026-01-02T03:04:05Z", "Datetime('2026-01-02T03:04:05Z')"},
		{"Datetime64", "1960-01-02T03:04:05Z", "Datetime64('1960-01-02T03:04:05Z')"},
		{"Timestamp", "2026-01-02T03:04:05.123456Z", "Timestamp('2026-01-02T03:04:05.123456Z')"},
		{"Timestamp64", "1960-01-02T03:04:05.123456Z", "Timestamp64('1960-01-02T03:04:05.123456Z')"},
		{"Interval", "PT26H", "Interval('P1DT2H')"}, {"Interval64", "P1000000D", "Interval64('P1000000D')"}, {"Decimal(10,2)", "-001.50", "Decimal('-1.5', 10, 2)"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			c := qt.New(t)
			value, err := columnsemantics.NativeDefault(&protocol.DeclaredColumn{Type: test.kind, DefaultSet: true, DefaultValue: test.value, Nullable: true}, test.kind, map[string]bool{"small_integer_defaults": true, "document_type_defaults": true})
			c.Assert(err, qt.IsNil)
			literal, err := columnsemantics.Literal(value)
			c.Assert(err, qt.IsNil)
			c.Assert(literal, qt.Equals, test.literal)
		})
	}
}

func TestNativeDefaultRefusal(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		column     *protocol.DeclaredColumn
	}{
		{"expression", "Int64", &protocol.DeclaredColumn{DefaultExpression: "1+2"}},
		{"range", "Int8", &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "128"}},
		{"missing small integer capability", "Int16", &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "3"}},
		{"missing document capability", "JsonDocument", &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "{}"}},
		{"serial literal", "BigSerial", &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, err := columnsemantics.NativeDefault(test.column, test.kind, nil)
			c.Assert(err, qt.IsNotNil)
			c.Assert(value, qt.IsNil)
		})
	}
}

func TestNativeDefaultAbsence(t *testing.T) {
	for _, column := range []*protocol.DeclaredColumn{{}, {DefaultSet: true, DefaultValue: "NULL"}} {
		c := qt.New(t)
		value, err := columnsemantics.NativeDefault(column, "Int64", nil)
		c.Assert(err, qt.IsNil)
		c.Assert(value, qt.IsNil)
	}
}

func TestNativeLiteralRejectsWrongScalar(t *testing.T) {
	for _, value := range []*Ydb.Value{{}, {Value: &Ydb.Value_TextValue{TextValue: "5"}}, {Value: &Ydb.Value_Int32Value{Int32Value: 5}, Items: []*Ydb.Value{{}}}} {
		c := qt.New(t)
		literal, err := columnsemantics.Literal(&Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32}}, Value: value})
		c.Assert(err, qt.IsNotNil)
		c.Assert(literal, qt.Equals, "")
	}
}
